# gmdb Design Document

A memory-mapped, multi-process, embedded key-value database for Go.

## Design Decisions

| Decision | Choice | Rationale |
|---|---|---|
| Data structure | B+tree on fixed-size pages | Only viable option for multi-process mmap |
| Concurrency | Single writer + N readers (MVCC/CoW) | Proven (LMDB), readers never block writer |
| File layout | Fixed-size pages (4KB–64KB, configurable, immutable after creation) | Matches OS page size, mmap-friendly |
| Value storage | Inline + overflow pages | Simple single read path, overflow for large values |
| Free space | Freelist B+tree | LMDB-style, tracks free pages per txn |
| Isolation | Dual meta pages + CoW | No WAL needed, atomic commit |
| Crash safety | CoW + atomic meta page swap | File is always consistent |
| Cross-process | Shared memory lock file | Reader table for tracking oldest active reader |
| mmap | MAP_SHARED read + pwrite() for writes | OS handles cache coherency |
| Key ordering | Lexicographic (byte-ordered) | Simple, general, no custom comparator needed |
| Byte order | Little-endian (fixed) | Portable across architectures |
| Checksums | Meta pages only | CoW protects data pages; meta checksum detects torn commits |
| API | Transaction-based | Explicit read/write txns |
| Namespaces | Named keyspaces | Multiple B+trees in one file |

## File Layout

The database is a single file, divided into fixed-size pages. All pages are the
same size (configurable at creation time, immutable after). Supported page sizes
are powers of 2 from 4KB to 64KB. Default: 4096 bytes (OS page size).

All multi-byte integers are stored in little-endian byte order.

```
+--------+--------+--------+--------+--------+--------+----
| Meta 0 | Meta 1 | Page 2 | Page 3 | Page 4 | Page 5 | ...
+--------+--------+--------+--------+--------+--------+----
```

### Page Types

Every page starts with a common header:

```
Page Header (16 bytes)
+----------+----------+----------+----------+
| PageID   | Type     | Count    | Overflow |
| uint64   | uint16   | uint16   | uint32   |
+----------+----------+----------+----------+
```

- **PageID**: The page number (offset = PageID * PageSize).
- **Type**: One of: Meta, Branch, Leaf, Freelist, Overflow.
- **Count**: Number of items (keys in branch, key/value pairs in leaf).
- **Overflow**: Number of contiguous overflow pages following this one (0 for
  single-page nodes).

#### Meta Page

Two meta pages exist at page 0 and page 1. They alternate — the writer always
updates the one NOT currently active. Each meta page contains:

```
Meta Page
+------------------+
| Page Header      |
+------------------+
| Magic            | uint32 - identifies file as gmdb
| Version          | uint32 - format version
| PageSize         | uint32 - page size in bytes
| Flags            | uint32 - reserved
| FreelistRoot     | uint64 - root page of freelist B+tree
| NumFreePages     | uint64 - total free pages
| KeyspaceRoot     | uint64 - root page of keyspace B+tree
| NumKeyspaces     | uint64 - number of keyspaces
| LastPageID       | uint64 - highest allocated page ID
| TxnID            | uint64 - transaction ID that wrote this meta
| Checksum         | uint64 - xxhash of all above fields
+------------------+
```

The active meta page is the one with the highest TxnID whose checksum is valid.
If a crash happens mid-write to the meta page, the checksum will be invalid and
the database falls back to the other meta page — which points to the previous
consistent state.

#### Branch Page (Internal B+tree Node)

Branch pages store keys and child page pointers. They do NOT store values.

```
Branch Page
+------------------+
| Page Header      |
+------------------+
| Ptr[0]           | uint64 - page ID of leftmost child
+------------------+
| Key[0] | Ptr[1]  | Key bytes + page ID
| Key[1] | Ptr[2]  | Key bytes + page ID
| ...              |
+------------------+
```

Keys are stored in sorted order. For a branch with N keys, there are N+1 child
pointers. The search is: if `target < Key[i]`, descend to `Ptr[i]`; if target
>= all keys, descend to `Ptr[N]`.

Key layout within the page:

```
Cell Directory (at start of data area, grows forward)
+----------+----------+
| Offset   | KeyLen   |    per cell: offset into page + key length
| uint16   | uint16   |
+----------+----------+

Key Data (packed from end of page, grows backward)
+----------+----------+
| Key bytes| ChildPtr |
|          | uint64   |
+----------+----------+
```

This layout allows binary search over the cell directory without parsing
variable-length keys.

#### Leaf Page

Leaf pages store the actual key-value pairs.

```
Leaf Page
+------------------+
| Page Header      |
+------------------+
| Cell Directory   | Array of (Offset uint16, CellFlags uint16)
| ...              |
+------------------+
|     free space   |
+------------------+
| ...              |
| KV Data N        | packed from end of page
| KV Data 1        |
| KV Data 0        |
+------------------+
```

Each cell in the data area:

```
KV Cell
+----------+----------+-----------+-----------+
| KeyLen   | ValueLen | Key bytes | Val bytes |
| uint16   | uint32   |           |           |
+----------+----------+-----------+-----------+
```

If a value is too large to fit in the leaf page, the CellFlags field in the cell
directory indicates it's an overflow reference.

CellFlags bit layout:

```
Bit 0:    Overflow (0 = inline value, 1 = overflow reference)
Bit 1:    Compressed (reserved, 0 for now)
Bit 2:    Encrypted (reserved, 0 for now)
Bits 3-7: Compression algorithm ID (reserved, 0 for now)
Bits 8-15: Reserved (must be 0)
```

Overflow reference format:

```
Overflow Reference (instead of inline value)
+----------+----------+-----------+----------+----------+
| KeyLen   | 0        | Key bytes | OvflPage | TotalLen |
| uint16   | uint32   |           | uint64   | uint64   |
+----------+----------+-----------+----------+----------+
```

#### Overflow Page

Overflow pages are contiguous runs of pages that store large values. The first
page in the run has the standard page header with `Overflow` set to the number
of additional pages. The rest is raw value bytes.

#### Freelist B+tree

Free pages are tracked in a dedicated B+tree (separate from user data). The
freelist B+tree maps `TxnID -> []PageID` — pages freed by a given transaction
can only be reused once no reader is still using that transaction's snapshot.

The writer checks the reader table (in shared memory) to find the oldest active
reader's TxnID. Any freelist entries with TxnID < oldest_reader are safe to
reclaim.

## Copy-on-Write (CoW) Transaction Model

### Write Transaction

1. Writer acquires exclusive write lock (flock on data file or mutex in shared
   memory).
2. Writer reads the active meta page to get current roots and TxnID.
3. For each modification (insert, update, delete):
   - Traverse the B+tree from root to leaf.
   - Copy each page along the path (don't modify in place).
   - Allocate new pages from the freelist (or extend the file).
   - Modified pages are written to their new locations via `pwrite()`.
4. The old pages along the modified path are added to the freelist under the
   new TxnID.
5. All dirty pages are written and `fdatasync()`'d.
6. The inactive meta page is updated with new root pointers, new TxnID, and
   checksum.
7. The meta page is `fdatasync()`'d. This is the atomic commit point.
8. Writer releases exclusive lock.

### Read Transaction

1. Reader acquires a slot in the reader table (shared memory) and records the
   current TxnID from the active meta page.
2. Reader traverses the B+tree using page pointers from that meta page. Because
   of CoW, all pages referenced by this TxnID are immutable — the writer will
   never modify them in place.
3. When done, the reader clears its slot in the reader table.

Readers never need locks on the data file. They never block writers. Writers
never block readers. The only contention point is the reader table slot
acquisition, which is a simple atomic CAS.

## Cross-Process Coordination

### Lock File Layout

A separate file (`<dbname>.lock`) is mmap'd as shared memory by all processes.

```
Lock File
+------------------------+
| Write Lock             | 1 byte (used with flock or futex)
| Padding                | 7 bytes
+------------------------+
| Reader Table           |
| +---------+----------+ |
| | TxnID   | PID      | | Slot 0
| | uint64  | uint32   | |
| | Padding | 4 bytes  | |
| +---------+----------+ |
| | TxnID   | PID      | | Slot 1
| | ...                 | |
| +---------+----------+ |
| | ...                 | | up to MaxReaders slots (default 126)
| +---------+----------+ |
+------------------------+
```

Total lock file size: 8 + (16 * MaxReaders) = 8 + 2016 = 2024 bytes (fits in
one page).

### Write Lock

The writer acquires an exclusive `flock()` on the data file (or a dedicated lock
file). Only one writer at a time, across all processes.

### Reader Table

- On `BeginRead()`: find an empty slot (TxnID == 0), atomically write the
  current meta TxnID and PID.
- On `EndRead()`: atomically set the slot's TxnID to 0.
- Stale reader detection: if a PID in the reader table is no longer alive
  (checked via `kill(pid, 0)` or `/proc/<pid>`), the slot can be reclaimed.

### Writer's Freelist Reclamation

Before reclaiming pages, the writer scans the reader table to find the minimum
active TxnID. Any pages freed by transactions with TxnID < min_active are safe
to reuse.

## mmap Strategy

### Read Path

All processes mmap the data file with:
```
MAP_SHARED | PROT_READ
```

Reads go directly through the mmap. No system calls, no copies. The OS page
cache serves the data.

### Write Path

The writer does NOT write through the mmap. Instead:
- Allocate new pages (from freelist or by extending the file).
- Write new page contents via `pwrite()`.
- `fdatasync()` to flush to disk.
- Update meta page via `pwrite()`.
- `fdatasync()` again.

This ensures crash safety — the mmap is never in a dirty/inconsistent state
from the writer's perspective.

### mmap Resizing

When the writer extends the file (allocates pages beyond the current mmap size),
readers need to remap. Options:

1. **Over-allocate virtual address space**: mmap a large region (e.g., 1TB of
   virtual space) upfront but only the file-backed portion is usable. As the
   file grows, the existing mapping covers the new pages automatically. This
   works on 64-bit systems. The unmapped region beyond the file size will
   SIGBUS if accessed, so readers must check `LastPageID` from the meta page.

2. **Remap on transaction start**: Each time a reader begins a transaction, it
   checks if the file has grown beyond its current mmap. If so, it remaps.
   This is the bbolt approach.

Option 1 (over-allocate) is simpler and avoids remapping. The database sets a
maximum database size at creation time (default 256GB, configurable). This only
reserves virtual address space, not physical memory.

## Keyspaces

The root meta page points to a "keyspace B+tree" — a B+tree whose keys are
keyspace names (byte strings) and whose values are keyspace descriptors:

```
Keyspace Descriptor
+----------+----------+----------+----------+
| Root     | Depth    | Count    | Flags    |
| uint64   | uint16   | uint64   | uint16   |
+----------+----------+----------+----------+
```

- **Root**: Page ID of this keyspace's B+tree root.
- **Depth**: Height of the B+tree (for optimization).
- **Count**: Number of key-value pairs.
- **Flags**: Reserved (e.g., for duplicate key support in the future).

Opening a keyspace within a transaction reads the descriptor from the keyspace
B+tree. Modifications to the keyspace update the descriptor (and its root)
which propagates up through the keyspace B+tree via CoW.

## API Surface

```go
// Open a database. Creates the file if it doesn't exist.
func Open(path string, opts *Options) (*DB, error)

// Options for opening a database.
type Options struct {
    // PageSize in bytes. Only used when creating a new database.
    // Must be a power of 2 in range [4096, 65536]. Default: 4096.
    // Ignored when opening an existing database (read from meta page).
    PageSize int

    // MaxDBSize is the maximum virtual address space to reserve.
    // Default: 256GB. Only affects mmap reservation, not disk usage.
    MaxDBSize int64

    // MaxReaders is the maximum number of concurrent reader slots.
    // Default: 126.
    MaxReaders int

    // FileMode for newly created files. Default: 0644.
    FileMode os.FileMode

    // ReadOnly opens the database in read-only mode.
    ReadOnly bool
}

// DB is a handle to an open database.
type DB struct { ... }

func (db *DB) Close() error

// View executes a read-only transaction.
func (db *DB) View(fn func(tx *Tx) error) error

// Update executes a read-write transaction.
func (db *DB) Update(fn func(tx *Tx) error) error

// Begin starts a transaction manually.
func (db *DB) Begin(writable bool) (*Tx, error)

// Tx is a database transaction.
type Tx struct { ... }

func (tx *Tx) Commit() error
func (tx *Tx) Rollback() error

// OpenKeyspace opens a named keyspace within this transaction.
// Creates it if it doesn't exist (write txn only).
func (tx *Tx) OpenKeyspace(name []byte, create bool) (*Keyspace, error)

// DeleteKeyspace deletes a named keyspace.
func (tx *Tx) DeleteKeyspace(name []byte) error

// Keyspace is a handle to a named keyspace within a transaction.
type Keyspace struct { ... }

func (ks *Keyspace) Get(key []byte) ([]byte, error)
func (ks *Keyspace) Put(key, value []byte) error
func (ks *Keyspace) Delete(key []byte) error

// Cursor for iterating over key-value pairs.
func (ks *Keyspace) Cursor() *Cursor

type Cursor struct { ... }

func (c *Cursor) First() (key, value []byte)
func (c *Cursor) Last() (key, value []byte)
func (c *Cursor) Next() (key, value []byte)
func (c *Cursor) Prev() (key, value []byte)
func (c *Cursor) Seek(key []byte) (key, value []byte)
```

## Implementation Modules

The implementation is organized into the following packages/files:

### 1. `page` — Page Types and Serialization

- Page header encoding/decoding.
- Branch page: cell directory, key lookup (binary search), insert/split.
- Leaf page: cell directory, KV lookup, insert/split, overflow references.
- Meta page: encode/decode/validate checksum.

### 2. `btree` — On-Disk B+tree Operations

- Search: traverse branch pages to find leaf, binary search within leaf.
- Insert: search + copy-on-write path from leaf to root, split if needed.
- Delete: search + copy-on-write, merge/rebalance if needed.
- Cursor: stateful iterator holding a stack of (pageID, index) pairs.
- All operations work on page byte slices (from mmap), never Go heap objects.

### 3. `freelist` — Free Page Management

- B+tree mapping TxnID -> page ID ranges.
- Allocate: find reclaimable pages (TxnID < oldest reader).
- Free: record pages under current TxnID.
- Extend: grow file when no free pages available.

### 4. `mmap` — Memory Mapping

- Platform-specific mmap/munmap (linux, darwin).
- Initial mapping with over-allocated virtual address space.
- File extension (ftruncate + the mapping covers it automatically).

### 5. `lock` — Cross-Process Coordination

- Lock file creation and mmap (shared memory).
- Writer lock (flock-based).
- Reader table: slot acquire/release, stale PID detection.
- Oldest-reader query for freelist reclamation.

### 6. `tx` — Transaction Management

- Read transaction: snapshot meta, acquire reader slot, provide read-only
  B+tree access.
- Write transaction: snapshot meta, acquire write lock, track dirty pages,
  CoW operations, commit (write pages + fsync + meta swap + fsync), rollback.

### 7. `db` — Top-Level Database

- Open/Close.
- Environment setup (mmap, lock file).
- Transaction lifecycle (Begin/Commit/Rollback, View/Update helpers).
- Keyspace management.

## Limits

### Page Size

Configurable at database creation time. Must be a power of 2 in the range
4096–65536 (4KB–64KB). Stored in the meta page and immutable after creation.
Default: 4096 bytes.

### Maximum Key Size

Determined by page size. A branch page must fit at least 2 keys to allow
splitting. The maximum key size is approximately `(PageSize - 40) / 2`:

| Page Size | Max Key Size (approx) |
|-----------|----------------------|
| 4KB       | ~1024 bytes          |
| 8KB       | ~2048 bytes          |
| 16KB      | ~4096 bytes          |
| 64KB      | ~16384 bytes         |

Enforced at `Put()` time. Keys exceeding the limit return an error.

### Maximum Value Size

Inline values are limited by available space in the leaf page. Values that
exceed this are automatically stored as overflow pages. There is no practical
upper limit on value size (bounded only by disk space and `MaxDBSize`).

## Checksums

Only meta pages carry checksums (xxhash64 of all fields). Data pages (branch,
leaf, overflow) do not have checksums.

**Rationale**: The meta page is the atomic commit point — a torn write here
would silently point to an inconsistent tree. The checksum detects this and
triggers fallback to the other meta page.

Data pages are protected by CoW: they are written to new locations and fsynced
before the meta page is updated. A crash during a data page write leaves the
meta page pointing to the old (consistent) tree. The half-written page is
orphaned and never referenced. Per-page checksums would only catch silent
bitrot after a successful write, which modern filesystems (ext4, ZFS, btrfs)
already detect.

## Integrity and Safety

- **No partial writes visible**: CoW ensures all modifications happen on new
  pages. The old tree is intact until the meta page swap.
- **Atomic commit**: A single meta page write (< page size, aligned) is the
  commit point. Even if it's torn, the checksum will fail and the DB falls
  back to the other meta page.
- **No fsync ordering violations**: dirty pages are fdatasync'd BEFORE the meta
  page update. The meta page is fdatasync'd AFTER writing it.
- **Reader isolation**: Readers see an immutable snapshot. Pages they reference
  cannot be reused until all readers on that TxnID have finished.
- **Stale reader recovery**: If a process crashes without releasing its reader
  slot, the PID-based detection allows the writer to reclaim the slot.

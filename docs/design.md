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
- **Type**: One of: Meta, Branch, Leaf, Overflow. (The freelist uses a regular
  B+tree with standard Branch and Leaf pages — no special page type needed.)
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
| Checksum         | uint64 - xxhash of all preceding bytes (header through TxnID)
+------------------+
```

Total meta page payload: 16 (header) + 4 + 4 + 4 + 4 + 8 + 8 + 8 + 8 + 8 +
8 + 8 = 88 bytes. Fits comfortably in any supported page size (min 4KB).

The active meta page is the one with the highest TxnID whose checksum is valid.
If a crash happens mid-write to the meta page, the checksum will be invalid and
the database falls back to the other meta page — which points to the previous
consistent state.

#### Branch Page (Internal B+tree Node)

Branch pages store keys and child page pointers. They do NOT store values.

```
Branch Page
+------------------------+
| Page Header (16 bytes) |
+------------------------+
| Ptr[0] (uint64)        |  leftmost child pointer (8 bytes)
+------------------------+
| Cell Directory         |  Array of (Offset uint16, KeyLen uint16)
| ...                    |  grows forward, 4 bytes per cell
+------------------------+
|       free space       |
+------------------------+
| ...                    |
| Cell Data 1            |  packed from end of page, grows backward
| Cell Data 0            |
+------------------------+
```

Each cell in the data area:

```
Branch Cell
+----------+----------+
| Key bytes| ChildPtr |
|          | uint64   |
+----------+----------+
```

Keys are stored in sorted order. For a branch with N cells (N keys), there are
N+1 child pointers: `Ptr[0]` (leftmost, stored after the page header) plus one
`ChildPtr` per cell.

Search algorithm: binary search the cell directory to find the first cell where
`target < Key[i]`. If found, descend to the child pointer of cell `i-1` (or
`Ptr[0]` if `i == 0`). If target >= all keys, descend to the last cell's
`ChildPtr`.

The cell directory stores `(Offset, KeyLen)` per cell, enabling binary search
over variable-length keys without parsing the key data area.

#### Leaf Page

Leaf pages store the actual key-value pairs. Note: the cell directory entry
format differs from branch pages — leaf cells use `(Offset, CellFlags)` instead
of `(Offset, KeyLen)`, because leaf cells encode `KeyLen` inside the cell data
itself and need the flags for overflow/compression/encryption metadata.

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
KV Cell (inline)
+----------+----------+-----------+-----------+
| KeyLen   | ValueLen | Key bytes | Val bytes |
| uint16   | uint32   |           |           |
+----------+----------+-----------+-----------+
```

`ValueLen` is uint32 (max ~4GB for inline values). In practice, inline values
are limited by leaf page free space — far below 4GB. Values that exceed leaf
page capacity are stored as overflow pages, referenced via the overflow format
below which uses uint64 `TotalLen` for unbounded value sizes.

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

Overflow reference format (used when CellFlags bit 0 is set):

```
Overflow Reference (instead of inline value)
+----------+-----------+----------+----------+
| KeyLen   | Key bytes | OvflPage | TotalLen |
| uint16   |           | uint64   | uint64   |
+----------+-----------+----------+----------+
```

The overflow cell has a different layout from the inline cell — there is no
`ValueLen` field. The reader checks `CellFlags.Overflow` to determine which
format to parse: inline (KeyLen + ValueLen + Key + Value) or overflow
(KeyLen + Key + OvflPage + TotalLen).

#### Overflow Page

Overflow pages are contiguous runs of pages that store large values. The first
page in the run has the standard page header with `Overflow` set to the number
of additional pages. The rest is raw value bytes.

#### Freelist B+tree

Free pages are tracked in a dedicated B+tree (separate from user data). Pages
freed by a given transaction can only be reused once no reader is still using
that transaction's snapshot.

The freelist B+tree uses a **composite key** encoding with empty values:

```
Key: TxnID (uint64, big-endian) || PageID (uint64, big-endian)
Value: (empty — zero bytes)
```

Each freed page is a separate entry in the B+tree. Both components are stored
in big-endian byte order so that the standard lexicographic key comparison
sorts entries first by TxnID, then by PageID within the same transaction.

This design has several advantages:
- **No special value encoding**: reuses the existing B+tree leaf format as-is.
  Each entry is just a 16-byte key with no value.
- **No overflow concern**: a single transaction freeing many pages creates many
  small entries rather than one large value that could overflow a leaf page.
- **Efficient range scan for reclamation**: to reclaim all pages freed by
  transactions older than the oldest active reader, the writer seeks to the
  beginning of the tree and scans forward until `TxnID >= oldest_reader`.
- **Simple allocation**: pop entries from the reclaimable range and delete them
  from the B+tree.

The writer checks the reader table (in shared memory) to find the oldest active
reader's TxnID. Any freelist entries with TxnID < oldest_reader are safe to
reclaim.

## Copy-on-Write (CoW) Transaction Model

### Write Transaction

1. Writer acquires exclusive write lock (flock on lock file).
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
| Header                 |
| Magic      | uint32    |  identifies file as gmdb lock file
| Version    | uint16    |  lock file format version
| MaxReaders | uint16    |  number of reader slots
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
| | ...                 | | up to MaxReaders slots
| +---------+----------+ |
+------------------------+
```

Header size: 8 bytes (aligned). Total lock file size: 8 + (16 * MaxReaders).
With default MaxReaders=126: 8 + 2016 = 2024 bytes (fits in one page).

The lock file is mmap'd with `MAP_SHARED` by all processes for the reader table.
The write lock is a separate concern handled via `flock()` (see below).

### Lock File Lifecycle

The lock file is ephemeral. The first process to open the database creates the
lock file and writes the header (including MaxReaders). Subsequent processes
read MaxReaders from the header and use it. If the lock file is deleted (e.g.,
after all processes exit), the next opener recreates it. MaxReaders is NOT
stored in the data file — it is a runtime coordination property, not a data
property.

### Write Lock

The writer acquires an exclusive `flock()` on the lock file. This is a
kernel-level advisory lock — no bytes in the file represent the lock state.
Only one writer at a time, across all processes.

### Reader Table

- On `BeginRead()`: scan the reader table for a slot with PID == 0. Atomically
  CAS the PID field from 0 to the caller's PID to claim the slot. If the CAS
  fails (another process claimed it), try the next slot. Once the slot is
  claimed, store the current meta TxnID into the slot's TxnID field.
- On `EndRead()`: set the slot's TxnID to 0, then set PID to 0. This order
  ensures the writer never sees a stale TxnID in an unclaimed slot.
- Stale reader detection: if a PID in the reader table is no longer alive
  (checked via `kill(pid, 0)` or `/proc/<pid>`), the slot can be reclaimed
  by setting both TxnID and PID to 0.

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

**Note**: Large virtual address reservations may be affected by Linux
`vm.overcommit_memory` settings or per-process `RLIMIT_AS` limits. On most
default configurations this is not an issue — the kernel distinguishes between
reserved virtual address space and committed memory. Users with restrictive
settings may need to lower `MaxDBSize`.

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

Total descriptor size: 8 + 2 + 8 + 2 = 20 bytes.

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
    // Default: 126. Only used when creating a new lock file.
    // Ignored when the lock file already exists (read from lock file header).
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
func (c *Cursor) Seek(target []byte) (key, value []byte)
```

## Implementation Layout

All code lives in a single `gmdb` package (flat, no sub-packages). This avoids
circular dependency issues between tightly coupled components (pages, B+tree,
transactions, mmap) and keeps the public API to one import path. The code is
organized by file:

| File | Responsibility |
|------|---------------|
| `page.go` | Page header encoding/decoding. Branch page: cell directory, key lookup (binary search), insert/split. Leaf page: cell directory, KV lookup, insert/split, overflow references. Meta page: encode/decode/validate checksum. |
| `btree.go` | B+tree search, insert (CoW path from leaf to root, split), delete (CoW, merge/rebalance). Cursor: stateful iterator holding a stack of (pageID, index) pairs. All operations work on page byte slices (from mmap), never Go heap objects. |
| `freelist.go` | B+tree with composite keys (TxnID \|\| PageID, big-endian, empty values). Allocate: scan reclaimable entries (TxnID < oldest reader), delete from tree. Free: insert entries for each freed page under current TxnID. Extend: grow file when no free pages available. |
| `mmap.go` | Platform-specific mmap/munmap. Initial mapping with over-allocated virtual address space. File extension (ftruncate + mapping covers it automatically). |
| `mmap_linux.go` | Linux mmap/munmap syscalls. |
| `mmap_darwin.go` | macOS mmap/munmap syscalls. |
| `lock.go` | Lock file creation and mmap (shared memory). Writer lock (flock-based). Reader table: slot acquire/release, stale PID detection. Oldest-reader query for freelist reclamation. |
| `tx.go` | Read transaction: snapshot meta, acquire reader slot, read-only B+tree access. Write transaction: snapshot meta, acquire write lock, track dirty pages, CoW operations, commit (write pages + fsync + meta swap + fsync), rollback. |
| `db.go` | Open/Close. Environment setup (mmap, lock file). Transaction lifecycle (Begin/Commit/Rollback, View/Update helpers). Keyspace management. |

## Limits

### Page Size

Configurable at database creation time. Must be a power of 2 in the range
4096–65536 (4KB–64KB). Stored in the meta page and immutable after creation.
Default: 4096 bytes.

### Maximum Key Size

Determined by page size. A branch page must fit at least 2 keys to allow
splitting. The fixed overhead is 24 bytes (16-byte page header + 8-byte
leftmost child pointer). Each key requires 4 bytes (cell directory entry) +
key bytes + 8 bytes (child pointer). The maximum key size is approximately
`(PageSize - 48) / 2`:

| Page Size | Max Key Size (approx) |
|-----------|----------------------|
| 4KB       | ~2024 bytes          |
| 8KB       | ~4024 bytes          |
| 16KB      | ~8024 bytes          |
| 64KB      | ~32744 bytes         |

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

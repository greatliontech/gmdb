# API Surface

The Go-level public API: sentinel errors, the `Options` struct, the
DB / Tx / Keyspace / Cursor / Index types and their methods,
`Check` / `CopyTo` / `Compact`, byte-slice ownership rules, nil and
empty semantics, database initialisation, path traversal safety.

This spec is the contract for callers. Storage, tree, and protocol
details live in the corresponding specs (`page-formats.md`,
`pager-slab.md`, etc.). The typed (generic) layer lives in
`typed-keyspaces.md`.

Scope:
- Sentinel error inventory.
- Error wrapper `IndexFingerprintError`.
- `Open()` constructor.
- Byte-slice ownership.
- Nil / empty semantics.
- Database initialisation state.
- Path-traversal safety via `os.OpenRoot`.
- Types and Options (`SyncMode`, `Options`, `FileFormat`,
  `LaggingReaderInfo`, `LaggingReaderAction`).
- DB and transaction API.
- Keyspace and SetKeyspace API.
- Cursor and SetCursor API surface (state-machine semantics live
  in `transactions.md`).
- Range iterators (`All`, `Range`, `Prefix`).
- Index lookup API.
- Statistics (`DBStats`, `TxStats`, `KeyspaceStats`,
  `IndexStats`).
- `Check`, `CheckWithOptions`, `CopyTo`, `Compact`.

Depends on / interacts with:
- All specs that the API implements; cross-referenced inline.

## Invariants

Invariant: kind=clause-explicit;
  property=Empty (`[]byte{}`) and `nil` keys are both **invalid**.
    Every operation taking a key returns `ErrKeyEmpty` if the
    key is nil or empty;
  from=this spec §Nil and Empty Semantics;
  violation=Accepting empty keys ambiguates the "no key found"
    sentinel with a real empty-key row — `Get(nil)` would
    legitimately need a code path indistinguishable from
    `ErrNotFound`.

Invariant: kind=clause-explicit;
  property=Empty (`[]byte{}`) values are valid; `nil` values
    are treated as empty. `Get(k)` returns `(nil, ErrNotFound)`
    iff the key is genuinely absent; it returns
    `([]byte{}, nil)` for a key whose stored value is empty;
  from=this spec §Nil and Empty Semantics + return-value table;
  violation=Conflating nil-return-with-no-error and empty-byte-
    slice breaks the documented "set of keys" pattern where a
    keyspace stores empty values.

Invariant: kind=clause-explicit;
  property=Every `[]byte` returned by gmdb is a **borrowed
    reference**. Value slices are valid until **transaction
    close** (`Commit()` or `Rollback()`). Key slices may be
    invalidated by the **next cursor operation**;
  from=this spec §Byte Slice Ownership;
  violation=A caller retaining a key past the next cursor op
    or a value past txn close reads either zero-filled bytes
    (pool clear) or another transaction's data (pool reuse).

Invariant: kind=clause-explicit;
  property=`Open()` opens both the data file and the lock file
    via `os.OpenRoot`-confined operations, rejecting symlink
    traversal outside the database directory;
  from=this spec §Path Traversal Safety;
  violation=A symlink at the database path causes `Open()` to
    create or overwrite files outside the intended directory
    — an attacker controlling the database path can redirect
    file operations.

Invariant: kind=clause-explicit;
  property=`Options.SyncMode = SyncUnsafe` is rejected at
    `Open()` unless `Options.AllowSyncUnsafe = true`;
  from=this spec §Types and Options + `durability.md`;
  violation=See `durability.md`.

Invariant: kind=clause-explicit;
  property=`tx.SetFileFormat()` rejects a change to
    `FileFormat.Upper` (alias `MaxSize`) that differs from
    the stored value with a non-nil error; `MinSize`,
    `GrowStep`, and `ShrinkThreshold` are accepted;
  from=this spec §Database and Transaction API;
  violation=See `file-format.md` (`MaxSize` immutability).

Invariant: kind=entailed;
  property=`Compact()` either succeeds — atomic-renaming the
    rebuilt file over the original on the same filesystem and
    reopening fds/mmap — or fails without changing the visible
    state of the database (no rename, no fd swap). A timeout
    waiting for active in-process readers to drain returns
    `ErrCompactReadersActive` and performs no copy or rename;
  from=entailed: this spec §Check, CopyTo, Compact;
  violation=A partial `Compact` that renamed the file but
    failed to reopen fds leaves the process with handles to
    the unlinked inode while other processes see the new
    inode — split-brain on a single database.

## Sentinel errors

```go
var (
    ErrNotFound                = errors.New("gmdb: key not found")
    ErrKeyExists               = errors.New("gmdb: key already exists")
    ErrDBFull                  = errors.New("gmdb: database full (MaxSize reached)")
    ErrTxTooLarge              = errors.New("gmdb: transaction too large")
    ErrReadersFull             = errors.New("gmdb: no reader slots available")
    ErrKeyTooLarge             = errors.New("gmdb: key exceeds maximum size")
    ErrKeyEmpty                = errors.New("gmdb: key is nil or empty")
    ErrCorrupted               = errors.New("gmdb: database corrupted")
    ErrBadPageChecksum         = errors.New("gmdb: page checksum mismatch")
    ErrVersionMismatch         = errors.New("gmdb: format version mismatch")
    ErrReadOnly                = errors.New("gmdb: write operation on read-only transaction")
    ErrTxClosed                = errors.New("gmdb: transaction already committed or rolled back")
    ErrPoisoned                = errors.New("gmdb: database handle is poisoned; Close and re-Open to recover")
    ErrClosed                  = errors.New("gmdb: database is closed")
    ErrCursorUnpositioned      = errors.New("gmdb: cursor not positioned")
    ErrKeyspaceKindMismatch    = errors.New("gmdb: keyspace kind does not match existing keyspace")
    ErrKeyspaceReserved        = errors.New("gmdb: keyspace name reserved for engine use")
    ErrValueSizeMismatch       = errors.New("gmdb: value size does not match fixed value size")

    // Indexing.
    ErrIndexExtractorRequired   = errors.New("gmdb: index extractor required for OpenKeyspace")
    ErrIndexUnknown             = errors.New("gmdb: IndexDecl supplied for index not declared in registry")
    ErrIndexFingerprintMismatch = errors.New("gmdb: index fingerprint mismatch — RebuildIndex required")
    ErrIndexUniqueViolation     = errors.New("gmdb: unique index violation")
    ErrIndexNotUnique           = errors.New("gmdb: Get called on non-unique index")
    ErrIndexExists              = errors.New("gmdb: index already exists")
    ErrIndexNotFound            = errors.New("gmdb: index not found")
    ErrIndexEncoderIDEmpty      = errors.New("gmdb: typed index encoder returned empty ID() — encoder IDs must be unique non-empty strings")

    // Keyspace lifecycle.
    ErrKeyspaceAlreadyOpen      = errors.New("gmdb: keyspace already opened in this transaction with a different index set")
    ErrKeyspaceClosed           = errors.New("gmdb: keyspace handle is invalid (keyspace deleted in this transaction)")

    // Compact.
    ErrCompactReadersActive     = errors.New("gmdb: Compact drain timed out — in-process read transactions still active")

    // BulkLoad.
    ErrBulkLoadOutOfOrder       = errors.New("gmdb: BulkLoad input not in ascending key order")
    ErrBulkLoadNonEmpty         = errors.New("gmdb: BulkLoad requires an empty keyspace")
)
```

## `IndexFingerprintError`

`ErrIndexFingerprintMismatch` is returned wrapped in an
`*IndexFingerprintError` whose fields name the drifted index
and distinguish schema-hash vs version-tag drift.

`Field` is the discriminant; callers MUST inspect `Field`
before reading the corresponding pair:

- `Field == "schema-hash"` → `StoredHash` and `SuppliedHash`
  are valid; `StoredVersion` and `SuppliedVersion` are empty
  strings (not meaningful).
- `Field == "version"` → `StoredVersion` and `SuppliedVersion`
  are valid; `StoredHash` and `SuppliedHash` are zero (not
  meaningful).

The zero values for the inactive pair are NOT a real
hash/version collision — they are sentinel placeholders.
Callers logging the error should branch on `Field`, not on
`uint64==0` or `string==""`.

```go
type IndexFingerprintError struct {
    Keyspace        string
    IndexName       string
    Field           string // "schema-hash" or "version"
    StoredHash      uint64 // valid when Field == "schema-hash"
    SuppliedHash    uint64 // valid when Field == "schema-hash"
    StoredVersion   string // valid when Field == "version"
    SuppliedVersion string // valid when Field == "version"
}

func (e *IndexFingerprintError) Error() string { /* ... */ }
func (e *IndexFingerprintError) Unwrap() error { return ErrIndexFingerprintMismatch }
```

## `ErrPoisoned`

`ErrPoisoned` is returned by `Begin` (and therefore `Update`) after
a previous write transaction's commit failed in the publication
phase: step-3 pwrite of the new meta page, or step-4 fdatasync of
the meta. See `pager-slab.md §Commit Write Ordering` for the
four-step protocol; the publication boundary is step 3.

After such a failure, the on-disk active meta may have advanced
to the new tree (step-3 success + step-4 EIO leaves the new meta
visible to a fresh `Open`), while the in-process pager's
in-memory bitmap, `HighWaterMark`, and RPL chain were restored
to pre-tx by `AbortTx`. The handle's view of the file is
therefore inconsistent with disk: a subsequent `AllocPage` off
this handle would draw from the stale bitmap and could hand back
a page the on-disk active tree already references — a subsequent
commit would then overwrite that page's content, and the next
`Open` (in this or another process) would see a tree pointing
at clobbered data.

`ErrPoisoned` directs the caller to discard the handle:

```go
err := db.Update(ctx, fn)
if errors.Is(err, gmdb.ErrPoisoned) {
    _ = db.Close()
    db, err = gmdb.Open(ctx, path, opts)
    // The re-opened handle reads everything from disk via the
    // same machinery cross-process Open uses after a writer
    // crash; it is internally consistent.
}
```

`Close` works normally on a poisoned handle (releases the mmap,
closes the file). Recovery is unconditionally Close + re-Open;
there is no in-process repair API in v0 (a future chunk-11
`Check()`-driven repair may offer one).

A poisoned handle's `Begin` returns `ErrPoisoned` without
acquiring the write lock, so a poisoned handle does not block
unrelated callers — they observe the sentinel and act on it.
The poison state is process-local; cross-process `Open` of the
same file is unaffected by another process's poisoned handle.

## `Open`

```go
// Open a database. Creates the file if it doesn't exist.
//
// The data file is created with O_CREATE|O_EXCL to prevent races when
// multiple processes call Open() simultaneously on a non-existent
// path. If exclusive create fails with EEXIST, Open() retries as a
// normal open. The lock file uses the same pattern.
func Open(path string, opts *Options) (*DB, error)
```

## Byte Slice Ownership

All `[]byte` slices returned by gmdb (from `Get`, `Cursor.Next`,
`Cursor.Seek`, etc.) are **borrowed references** — they point
into either the mmap, the writer's slab buffer (when reading
own writes in a write txn), or an internal cursor buffer. The
caller does not own them.

**Value slices** point directly into the mmap (for inline values
from committed pages) or into overflow pages in the mmap, or
into the writer's slab buffer (for inline values from same-txn
modifications). Valid until the **transaction closes**
(`Commit()` or `Rollback()`).

**Key slices** may point into the mmap (for keys at restart
points in prefix-compressed leaves), into a slab buffer, or
into the cursor's key reconstruction buffer (`keyBuf`). The
reconstruction buffer is reused on each cursor movement. Key
slices are valid until the **next cursor operation** or
transaction close, whichever comes first.

**Slab buffer lifetime guarantee.** Within a write transaction,
a value or key slice that points into a slab buffer (own-writes
read path) remains valid for the entire transaction even if
the page that buffer represented is subsequently CoW'd,
rebalanced, or freed within the same transaction. See
`pager-slab.md §Slab Budget and ErrTxTooLarge`.

Callers who need a key or value to outlive these scopes must
copy it:

```go
k, v := c.Next()
savedKey := bytes.Clone(k)
savedVal := bytes.Clone(v)
```

`Keyspace.Get()` returns a value slice; valid until transaction
close.

This contract is the standard for mmap-based B+tree databases
(LMDB, libmdbx, BoltDB). Zero-copy reads are a core performance
property.

## Nil and Empty Semantics

**Keys:** empty (`[]byte{}`) and nil keys are both **invalid**.
Any operation taking a key returns `ErrKeyEmpty` if the key is
nil or empty.

**Values:** empty (`[]byte{}`) are **valid**. A key can exist
with no associated data — useful for using a keyspace as a set
of keys. Nil values are treated as empty: `Put(key, nil)`
stores a zero-length value.

**Return value conventions:**

| Call | Key exists (empty value) | Key exists (non-empty) | Key not found | End of iteration |
|------|--------------------------|------------------------|---------------|------------------|
| `Keyspace.Get(k)` | `([]byte{}, nil)` | `(value, nil)` | `(nil, ErrNotFound)` | N/A |
| `Cursor.Next()` | `(key, []byte{})` | `(key, value)` | N/A | `(nil, nil)` |
| `Cursor.Err()` | — | — | — | non-nil if iteration ended due to error |

Nil return from `Get` always means "not found" with
`ErrNotFound`. Nil return from cursor navigation always means
"end of iteration" (check `Err()` to distinguish normal end
from error). Empty `[]byte{}` return from `Get` means "key
exists, value is empty."

## Database Initialization

When `Open()` creates a new database:

1. Create the data file with `O_CREATE|O_EXCL`. If `EEXIST`,
   retry as normal open.
2. Write both meta pages identically:
   - `TxnID = 0`
   - `HighWaterMark = 2 + BitmapPages`
   - `KeyspaceRoot = 0` (empty)
   - `NumKeyspaces = 0`
   - `RPLHeadPage = 0`, `RPLTailPage = 0`, `RPLEntryCount = 0`
   - `NumFreePages = 0`
   - Checkpoint flag set on both
   - File-format fields from `Options.FileFormat` (or defaults)
   - `UUID` via `crypto/rand`
3. Initialize the bitmap region: all bits clear.
4. Create the lock file with `O_CREATE|O_EXCL`, matching UUID,
   empty reader table.
5. `fdatasync` the data file.

The first write transaction increments `TxnID` to 1.

## Path Traversal Safety

`Open()` uses `os.OpenRoot` (Go 1.24+) to confine file
operations to the database directory:

```go
root, err := os.OpenRoot(filepath.Dir(path))
defer root.Close()
dataFile, err := root.Open(filepath.Base(path), ...)
lockFile, err := root.Open(filepath.Base(path)+".lock", ...)
```

`os.OpenRoot` rejects symlink traversal outside the root
directory. Prevents an attacker who controls the database path
from redirecting file operations to arbitrary locations via
symlinks. Without this, a symlink at the database path could
cause `Open()` to create or overwrite files outside the
intended directory.

Used for both the data file and the lock file during `Open()`.
After return, resolved fds are used directly and the `os.Root`
is closed.

## Types and Options

```go
// SyncMode controls the durability guarantees of committed transactions.
type SyncMode int

const (
    SyncDurable    SyncMode = iota // syncs data + meta. Full ACID. Default.
    SyncDataOnly                   // syncs data; not meta. Last txn may be lost on crash.
    SyncLazy                       // skips all syncs. Rolls back to last Checkpoint() on crash.
    SyncUnsafe                     // skips all syncs, no safety net. Requires AllowSyncUnsafe.
)

// Options for opening a database.
type Options struct {
    // PageSize in bytes. Only used when creating a new database.
    // Must be a power of 2 in [4096, 65536]. Default: 4096.
    PageSize int

    // PageChecksum enables xxhash64 footers on data pages. Stored as
    // a flag in the meta page — immutable after creation. Default: true.
    // Only used when creating; ignored when opening existing.
    PageChecksum bool

    // FileFormat controls database file size bounds and growth.
    // Only used when creating; modify via Tx.SetFileFormat() at runtime.
    FileFormat FileFormat

    // SyncMode controls durability. Default: SyncDurable.
    SyncMode SyncMode

    // AllowSyncUnsafe must be true when using SyncUnsafe mode.
    // Without it, Open() returns an error when SyncMode = SyncUnsafe.
    // Default: false.
    AllowSyncUnsafe bool

    // MaxReaders is the maximum number of concurrent reader slots.
    // Default: 4096. Only used when creating a new lock file.
    // Type is `uint32` to match the lock-file header field
    // (cross-process.md §Lock File Layout `LockFileHeader.MaxReaders`)
    // and to carry the [1, 65536] bound at the type level (no
    // runtime int→uint32 conversion needed).
    MaxReaders uint32

    // MaxTxBufferBytes bounds the per-write-transaction slab (live +
    // loose + commit-time assembly buffers). A write transaction
    // that dirties more pages than this fails the next CoW (or
    // step 0 of commit) with ErrTxTooLarge.
    //
    // Sizing guide: each Put/Delete on an indexed keyspace with I
    // indexes can CoW up to depth × (I + 1) pages in the worst case
    // (row tree + each index tree, one CoW per level). At 4 KB
    // pages, depth 5, and 3 indexes: ~80 KB of unique CoW
    // destinations per maximally-touching Put; the 256 MiB default
    // accommodates ~3,000–3,200 such Puts before ErrTxTooLarge. For
    // larger workloads, use BulkLoad (which bypasses the slab via
    // streaming pwrite) or chunk the work across multiple write
    // transactions. Default: 256 MiB.
    MaxTxBufferBytes int64

    // RestartGroupTarget is the engine-wide default for the leaf
    // prefix-compression restart interval. Per-keyspace overrides via
    // Tx.SetKeyspaceConfig(). Default: 16.
    RestartGroupTarget int

    // MergeThreshold is the B+tree page fill percentage below which a
    // page is merged with a sibling after deletion. Range: 1-50.
    // Default: 25.
    MergeThreshold int

    // LaggingReader is called when a long-lived reader is blocking
    // RPL reclamation during page allocation. If nil, pageAlloc()
    // falls through to file extension when reclamation is blocked.
    LaggingReader func(info LaggingReaderInfo) LaggingReaderAction

    // MaxBatchSize is the maximum number of Batch() calls collected
    // before executing in one transaction. Default: 1000.
    MaxBatchSize int

    // MaxBatchDelay is the maximum time to wait for additional
    // Batch() calls before executing the current batch. Set to 0 to
    // fire immediately. Default: 10ms.
    MaxBatchDelay time.Duration

    // StaleTimeout for cross-PID-namespace stale detection via
    // heartbeats. Default: 10s.
    StaleTimeout time.Duration

    // HeartbeatInterval is how often the heartbeat goroutine
    // refreshes `WriterHeartbeat` (while this process holds
    // `LOCK_EX`) and the `Heartbeat` field of every active reader
    // slot. Must be significantly less than `StaleTimeout` for
    // scheduling jitter. Default: 1s. See cross-process.md §Heartbeat
    // Goroutine.
    HeartbeatInterval time.Duration

    // LockRetryInterval is the polling interval the flock goroutine
    // uses when flock(LOCK_EX|LOCK_NB) returns EWOULDBLOCK. Bounds
    // both Close() shutdown latency and per-writer ctx cancellation
    // latency under cross-process write-lock contention. Default: 50ms.
    LockRetryInterval time.Duration

    // Logger for diagnostic messages. If nil, discarded.
    Logger *slog.Logger

    // FileMode for newly created files. Default: 0644.
    FileMode os.FileMode

    // PreloadPages calls madvise(MADV_POPULATE_READ) at open
    // (Linux 5.14+). Default: false.
    PreloadPages bool

    // HugePages calls madvise(MADV_HUGEPAGE) on the data mmap
    // (Linux). Default: false.
    HugePages bool

    // ReclaimOnClose calls madvise(MADV_COLD) on the accessed mmap
    // region when a read transaction closes (Linux 5.4+).
    // Default: false.
    ReclaimOnClose bool

    // ReadOnly opens the database in read-only mode: lock file not
    // opened for write, flock goroutine not started, write
    // transactions return ErrReadOnly. The data mmap is always
    // PROT_READ regardless.
    //
    // When ReadOnly is true and the data file does not exist, Open()
    // returns the underlying os.ErrNotExist (it never creates a
    // database in read-only mode — that would be a contradiction).
    // Default: false.
    ReadOnly bool

    // ScratchDir is the directory used for BulkLoad sort spill on
    // indexed keyspaces. Must be on the same filesystem as the
    // database file when Compact() is used (atomic rename
    // requirement). Default: os.TempDir().
    ScratchDir string

    // CompactDrainTimeout bounds how long Compact() waits for active
    // in-process read transactions to commit/rollback before
    // proceeding with the copy. Exceeded → Compact returns
    // ErrCompactReadersActive without doing any work. Default: 30s.
    CompactDrainTimeout time.Duration

    // Maintenance controls the background maintenance goroutine.
    // If nil, defaults are used (maintenance enabled, 5m interval).
    Maintenance *MaintenanceOptions
}

// FileFormat controls file size bounds and growth/shrink behavior.
// All sizes are in bytes and must be multiples of PageSize.
type FileFormat struct {
    // Lower is the minimum file size in bytes. File never shrinks below.
    // Default: (2 + BitmapPages) * PageSize.
    Lower uint64

    // Upper is the maximum file size in bytes. Determines mmap
    // reservation size and bitmap size. Must be a multiple of PageSize.
    // Immutable after creation. Default: 256 GiB.
    Upper uint64

    // GrowStep is the number of bytes to grow by when extending.
    // Must be a multiple of PageSize. Default: 256 MiB.
    GrowStep uint64

    // ShrinkThreshold is the minimum unused bytes at file tail before
    // shrink occurs. Must be a multiple of PageSize. Default: 512 MiB.
    ShrinkThreshold uint64
}

// LaggingReaderInfo describes a reader blocking RPL reclamation.
type LaggingReaderInfo struct {
    PID       uint64
    TxnID     uint64
    Lag       uint64 // number of transactions behind current
    HeldPages uint64 // estimated pages held unreclaimable
}

type LaggingReaderAction int

const (
    LaggingReaderWait  LaggingReaderAction = iota // retry; reader may release
    LaggingReaderAbort                            // abort with ErrDBFull
)
```

## Database and Transaction API

```go
// DB is a handle to an open database.
type DB struct { ... }

func (db *DB) Close() error

// Checkpoint flushes all outstanding writes to stable storage. In
// SyncLazy mode this creates a checkpoint (database will roll back to
// this point at most on crash). In SyncDurable/SyncDataOnly modes,
// no-op (commits already sync). In SyncUnsafe, syncs but does not
// retroactively fix ordering from prior commits.
//
// Checkpoint acquires the write lock for its duration via the flock
// goroutine's FIFO queue; it serializes with concurrent write
// transactions and Compact(). Concurrent reads are not affected.
//
// The context governs the wait for the write lock — if Compact() is
// running ahead of this call in the queue, the wait can be long
// (Compact takes the lock for CopyTo's full duration). Callers on a
// timer (periodic Checkpoint in a service) should pass a context
// with a deadline. Once Checkpoint has the lock, ctx is not checked
// further — the fsync + pwrite sequence completes unconditionally
// (it is bounded and short relative to a Compact wait).
func (db *DB) Checkpoint(ctx context.Context) error

// View executes a read-only transaction. The context governs slot
// acquisition only — once the callback is entered, the context is not
// checked by the engine. Long-scan cancellation is a caller concern:
// the supplied fn can capture ctx and poll it (ctx.Err()) at natural
// break points (e.g., between cursor pages, between key ranges) and
// return early if cancelled. For request-driven services, the right
// pattern is one short View per request, not a long View polled for
// cancellation.
func (db *DB) View(ctx context.Context, fn func(tx *Tx) error) error

// Update executes a read-write transaction.
func (db *DB) Update(ctx context.Context, fn func(tx *Tx) error) error

// Batch submits a write operation to be batched with other concurrent
// callers into a single transaction. The context governs the wait for
// batch inclusion. Each closure runs in its own child transaction and
// executes exactly once. See `transactions.md §Write Batching`.
//
// The closure MUST NOT call Commit() or Rollback() on the supplied
// *Tx — the batch coordinator owns child-transaction lifecycle. A
// closure that calls either causes the coordinator's subsequent
// child-commit-or-rollback to error with ErrTxClosed, which
// propagates to the caller as the closure's result.
func (db *DB) Batch(ctx context.Context, fn func(tx *Tx) error) error

// Begin starts a transaction manually. The context governs lock/slot
// acquisition; once Begin returns a *Tx the context is not stored.
func (db *DB) Begin(ctx context.Context, writable bool) (*Tx, error)

// Tx is a database transaction.
type Tx struct { ... }

func (tx *Tx) Commit() error
func (tx *Tx) Rollback() error

// BeginChild creates a child transaction within the current write
// transaction. Children can be committed (merged into parent) or
// rolled back (discarded) independently. Only valid on a write txn.
func (tx *Tx) BeginChild() (*Tx, error)

// SetFileFormat updates the file format. MaxSize is immutable and
// cannot be changed; returns an error if FileFormat.Upper differs.
// Only valid on a write transaction.
func (tx *Tx) SetFileFormat(f FileFormat) error
```

## Keyspace API

```go
// OpenKeyspace opens an existing single-value keyspace for read+write.
// Every declared index on the keyspace must be supplied as an
// IndexDecl. Missing indexes return ErrIndexExtractorRequired; extras
// return ErrIndexUnknown; drifted fingerprints return
// ErrIndexFingerprintMismatch.
func (tx *Tx) OpenKeyspace(name string, indexes ...*IndexDecl) (*Keyspace, error)

// OpenKeyspaceReadOnly opens an existing keyspace for reads only.
// No IndexDecls required (and none accepted). Index lookups still work.
func (tx *Tx) OpenKeyspaceReadOnly(name string) (*Keyspace, error)

// CreateKeyspace creates a new single-value keyspace and (optionally)
// declares indexes. Returns ErrKeyExists if the keyspace exists.
func (tx *Tx) CreateKeyspace(name string, indexes ...*IndexDecl) (*Keyspace, error)

// CreateKeyspaceIfNotExists opens the keyspace if it exists (matching
// indexes required) or creates it (with the supplied indexes).
// ErrKeyspaceKindMismatch if it exists as a SetKeyspace.
func (tx *Tx) CreateKeyspaceIfNotExists(name string, indexes ...*IndexDecl) (*Keyspace, error)

// OpenSetKeyspace, OpenSetKeyspaceReadOnly, CreateSetKeyspace,
// CreateSetKeyspaceIfNotExists follow the same pattern.
type SetKeyspaceOptions struct {
    FixedValueSize int
}

func (tx *Tx) OpenSetKeyspace(name string, indexes ...*IndexDecl) (*SetKeyspace, error)
func (tx *Tx) OpenSetKeyspaceReadOnly(name string) (*SetKeyspace, error)
func (tx *Tx) CreateSetKeyspace(name string, opts *SetKeyspaceOptions, indexes ...*IndexDecl) (*SetKeyspace, error)
func (tx *Tx) CreateSetKeyspaceIfNotExists(name string, opts *SetKeyspaceOptions, indexes ...*IndexDecl) (*SetKeyspace, error)

// DeleteKeyspace removes a keyspace and everything reachable from
// its descriptor as a single atomic CoW operation. Three sub-trees
// are retired together:
//
//   1. The keyspace's own B+tree (row data, including SetKeyspace
//      nested B+trees for value sets) — bulk subtree retirement.
//   2. Each engine-internal index keyspace (Kind=2) referenced from
//      the per-keyspace index registry — bulk subtree retirement
//      per index.
//   3. The per-keyspace index registry sub-tree itself (rooted at
//      IndexRegistryRoot in the keyspace descriptor) — bulk
//      subtree retirement.
//
// All three retirements happen inside the same write transaction.
// The keyspace descriptor is then removed from the keyspace B+tree
// (which propagates CoW to the meta page's KeyspaceRoot). On commit,
// the meta swap publishes all of (1)+(2)+(3)+descriptor-removal
// atomically; a mid-DeleteKeyspace crash leaves the prior meta
// active and none of the work visible.
//
// For indexed keyspaces, no per-row extractor call is needed because
// no index entries survive after step (2).
//
// Errors: ErrNotFound if the keyspace does not exist.
// ErrKeyspaceReserved if the supplied name is an engine-internal
// index keyspace (Kind=2 — not enumerable, not user-deletable).
//
// Any Keyspace/SetKeyspace/Cursor/Index handle previously opened on
// the named keyspace within this transaction is invalidated by
// DeleteKeyspace — subsequent operations on those handles return
// ErrKeyspaceClosed. **Re-creating the keyspace in the same
// transaction via CreateKeyspace does NOT reactivate the old
// handle**: invalidation is permanent for the handle's lifetime.
// The new CreateKeyspace returns a fresh *Keyspace; the old handle
// stays dead until it is dropped by the caller.
func (tx *Tx) DeleteKeyspace(name string) error

// ListKeyspaces returns the names of all user keyspaces (Kind=0
// Keyspace or Kind=1 SetKeyspace). Engine-internal index keyspaces
// (Kind=2) are filtered out — they are addressable only via their
// parent keyspace's index registry, not by name.
func (tx *Tx) ListKeyspaces() ([]string, error)

// SetKeyspaceConfig updates mutable per-keyspace settings.
// Currently only RestartGroupTarget. Returns an error for invalid
// values (e.g. RestartGroupTarget = 0 means engine default).
// Only valid on a write transaction.
func (tx *Tx) SetKeyspaceConfig(name string, cfg KeyspaceConfig) error

type KeyspaceConfig struct {
    RestartGroupTarget uint16 // 0 = leave unchanged
}

// RebuildIndex drops and re-populates the named index using the
// supplied IndexDecl (whose Name must match an existing registry
// entry on the keyspace). Bypasses the open-time fingerprint check;
// this is the recovery path after ErrIndexFingerprintMismatch.
// Blocking — runs inside the current write transaction. See the
// `indexing.md §Rebuild` section for the recovery pattern.
func (tx *Tx) RebuildIndex(keyspace string, decl *IndexDecl) error

// DropIndex removes the named index entirely.
func (tx *Tx) DropIndex(keyspace, indexName string) error

// Keyspace is a handle to a named single-value keyspace.
type Keyspace struct { ... }

func (ks *Keyspace) Get(key []byte) ([]byte, error)
func (ks *Keyspace) Put(key, value []byte) error
func (ks *Keyspace) Delete(key []byte) error
func (ks *Keyspace) DeleteRange(start, end []byte) (uint64, error)
func (ks *Keyspace) NextSequence() (uint64, error)
func (ks *Keyspace) Cursor() *Cursor

// Index returns a handle for querying the named index on this
// keyspace. Returns ErrIndexNotFound if no index with this name is
// registered. See `indexing.md §Lookup API` for query semantics.
func (ks *Keyspace) Index(name string) (*Index, error)

func (ks *Keyspace) BulkLoad(yield func(yield func(key, value []byte) bool)) (uint64, error)

// Cursor for iterating over key-value pairs. See `transactions.md
// §Cursor State Machine` for state semantics, Delete-post-state
// rules, and invalidation conditions.
type Cursor struct { ... }

func (c *Cursor) First() (key, value []byte)
func (c *Cursor) Last() (key, value []byte)
func (c *Cursor) Next() (key, value []byte)
func (c *Cursor) Prev() (key, value []byte)
func (c *Cursor) Seek(target []byte) (key, value []byte)
func (c *Cursor) SeekGE(target []byte) (key, value []byte)
func (c *Cursor) Current() (key, value []byte)

// Delete removes the current entry. Cursor must be Positioned;
// otherwise returns ErrCursorUnpositioned. After delete, advances
// to the next entry or transitions to End-of-iteration. Possible
// errors: ErrCursorUnpositioned, ErrReadOnly (on a read-only
// transaction or keyspace handle), ErrTxClosed, ErrKeyspaceClosed
// (parent keyspace deleted), ErrIndexUniqueViolation (only on
// indexed keyspaces if the engine's bookkeeping discovers an
// inconsistency).
func (c *Cursor) Delete() error

func (c *Cursor) Err() error

// SetKeyspace handle to a named set keyspace.
type SetKeyspace struct { ... }

func (ks *SetKeyspace) Has(key []byte) (bool, error)
func (ks *SetKeyspace) HasValue(key, value []byte) (bool, error)
func (ks *SetKeyspace) Put(key, value []byte) error
func (ks *SetKeyspace) Delete(key []byte) error
func (ks *SetKeyspace) DeleteValue(key, value []byte) error
func (ks *SetKeyspace) CountValues(key []byte) (uint64, error)
func (ks *SetKeyspace) DeleteRange(start, end []byte) (uint64, error)
func (ks *SetKeyspace) NextSequence() (uint64, error)
func (ks *SetKeyspace) Cursor() *SetCursor
func (ks *SetKeyspace) Index(name string) (*Index, error)
func (ks *SetKeyspace) BulkLoad(yield func(yield func(key, value []byte) bool)) (uint64, error)

// SetCursor for iterating over set keyspace key-value pairs.
type SetCursor struct { ... }

// Core navigation (same as Cursor).
func (c *SetCursor) First() (key, value []byte)
func (c *SetCursor) Last() (key, value []byte)
func (c *SetCursor) Next() (key, value []byte)
func (c *SetCursor) Prev() (key, value []byte)
func (c *SetCursor) Seek(target []byte) (key, value []byte)
func (c *SetCursor) SeekGE(target []byte) (key, value []byte)
func (c *SetCursor) Current() (key, value []byte)
func (c *SetCursor) Delete() error
func (c *SetCursor) Err() error

// Value navigation (within current key's set).
func (c *SetCursor) FirstValue() []byte
func (c *SetCursor) LastValue() []byte
func (c *SetCursor) NextValue() (value []byte)
func (c *SetCursor) PrevValue() (value []byte)
func (c *SetCursor) NextKey() (key, value []byte)
func (c *SetCursor) PrevKey() (key, value []byte)
func (c *SetCursor) SeekValue(target []byte) (value []byte)
func (c *SetCursor) CountValues() (uint64, error)
```

## Range Iterators

```go
// Read-only iterators on Keyspace.
func (ks *Keyspace) All() iter.Seq2[[]byte, []byte]
func (ks *Keyspace) Range(start, end []byte) iter.Seq2[[]byte, []byte]
func (ks *Keyspace) Prefix(prefix []byte) iter.Seq2[[]byte, []byte]

// On SetKeyspace, each (key, value) pair yields separately.
func (ks *SetKeyspace) All() iter.Seq2[[]byte, []byte]
func (ks *SetKeyspace) Range(start, end []byte) iter.Seq2[[]byte, []byte]
func (ks *SetKeyspace) Prefix(prefix []byte) iter.Seq2[[]byte, []byte]
```

## Index Lookup API

```go
type Index struct { /* unexported */ }

// Lookup returns (pk, value) pairs matching the exact column tuple.
// value is read from the index's covering bytes when the index covers
// the requested column set; otherwise via back-lookup to the row
// keyspace. Iteration ends when no more matches; check Err() for
// errors.
//
// Intra-transaction consistency: index cursor and back-lookup both
// read the current transaction's dirty state. Row writes and index
// updates happen atomically in the same Put/Delete/Cursor.Delete,
// so a back-lookup for an index entry always finds the row. If a
// back-lookup ever fails to find its PK (engine bug or external
// corruption), the entry is silently skipped from iteration and the
// inconsistency is reportable via Check().
func (idx *Index) Lookup(cols ...[]byte) iter.Seq2[[]byte, []byte]

// LookupKeys returns matching primary keys without back-lookup or
// covering decode. Iteration cost is O(matches) leaf scans only.
// Because LookupKeys never probes the row keyspace, it does not
// observe missing-PK inconsistencies (the silent-skip case noted
// on Lookup) — every index entry yields its raw PK, even if the
// corresponding row has somehow vanished. Use Check() for
// row/index consistency verification.
func (idx *Index) LookupKeys(cols ...[]byte) iter.Seq[[]byte]

// Range returns matches in [start, end). Each tuple is a slice of
// per-column byte slices; nil tuple = open-ended.
func (idx *Index) Range(start, end [][]byte) iter.Seq2[[]byte, []byte]

// Prefix returns matches whose leading columns match the prefix.
func (idx *Index) Prefix(leadingCols ...[]byte) iter.Seq2[[]byte, []byte]

// Get is shorthand for unique indexes: returns the single (pk, value)
// or ErrNotFound. Returns ErrIndexNotUnique when called on a
// non-unique index.
func (idx *Index) Get(cols ...[]byte) (pk, value []byte, err error)

// Err returns the first error encountered during the last sequence
// returned by Lookup / Range / Prefix.
//
// Index handles are not safe for concurrent use by multiple
// goroutines. The Err state is per-handle, so two overlapping
// iterators on the same *Index would race. Open the keyspace in
// separate transactions, or call ks.Index(name) once per goroutine,
// for concurrent index queries.
func (idx *Index) Err() error

// Stats returns the index's persistent count + tree statistics.
func (idx *Index) Stats() (IndexStats, error)
```

## Statistics

```go
type DBStats struct {
    FreePages     uint64
    RetiredPages  uint64
    FileSize      uint64
    MinSize       uint64
    MaxSize       uint64
    HighWaterMark uint64
    // ActiveReaders is a non-atomic scan of the lock-file reader
    // table (cluster-wide). The count can be off by ±N for N reader
    // transitions in flight during the scan. Use for metrics and
    // health diagnostics only — never as a synchronization barrier
    // ("ActiveReaders == 0" does NOT imply no reads are starting).
    ActiveReaders int
    MaxReaders    uint32

    // SlabBytes reports slab usage for THIS PROCESS's current write
    // transaction (0 when no write txn is open in this process).
    // Cross-process writer slab usage is not visible from any one DB
    // handle — only the holder of the cross-process write lock has a
    // local view of it. Aggregate cluster-wide slab usage is not
    // tracked.
    SlabBytes int64
}

func (db *DB) Stats() DBStats

type TxStats struct {
    CowPages       uint64
    LoosePages     uint64
    ReclaimedPages uint64
    WrittenPages   uint64 // data + bitmap + meta pages pwritten at commit

    // SlabPeakBytes is the maximum slab usage observed during the
    // transaction's lifetime. Useful for tuning MaxTxBufferBytes.
    //
    // Reset behavior is a deliberate choice, not a forced contract:
    // Rollback resets the value to 0 because the rolled-back work is
    // not representative of steady-state need (rollbacks are
    // exceptional and the peak they reach should not influence
    // tuning); Commit preserves it so the caller can read it
    // immediately before the *Tx becomes invalid. Tooling that
    // wants visibility into rolled-back peaks should snapshot
    // SlabPeakBytes from a Stats() call inside the txn before
    // calling Rollback.
    SlabPeakBytes int64

    Gets    uint64
    Puts    uint64
    Deletes uint64
    Splits  uint64
    Merges  uint64

    // Indexing.
    IndexEntriesInserted uint64
    IndexEntriesDeleted  uint64
    IndexUniqueProbes    uint64

    Duration time.Duration
}

func (tx *Tx) Stats() TxStats

type KeyspaceStats struct {
    Depth         int
    BranchPages   uint64
    LeafPages     uint64
    OverflowPages uint64
    Entries       uint64
    IndexCount    int
}

func (ks *Keyspace) Stats() (KeyspaceStats, error)
func (ks *SetKeyspace) Stats() (KeyspaceStats, error)

type IndexStats struct {
    Depth         int
    BranchPages   uint64
    LeafPages     uint64
    Entries       uint64
    Unique        bool
    Covering      bool
    SizeBytes     uint64
}
```

## Check, CopyTo, Compact

```go
type CheckSeverity int

const (
    CheckWarning CheckSeverity = iota // non-critical (e.g., suboptimal layout)
    CheckError                        // structural integrity violation
    CheckFatal                        // walk could not continue past this point
)

type CheckIssue struct {
    Severity CheckSeverity
    // Code is a stable, machine-parseable token for the issue class
    // (e.g., "BitmapLeak", "CheckIndexes.KeyspaceNotFound",
    // "BadPageChecksum", "RPLChainBroken"). Stable across gmdb
    // versions for the purposes of tooling that pattern-matches on
    // issues; new codes may be added but existing ones never change
    // meaning. Use Code for programmatic decisions; use Message for
    // human-facing display.
    Code     string
    PageID   uint64
    Keyspace string
    Index    string // empty for non-index issues
    // Message is a human-readable description of the issue. Free-form
    // and free to change between versions; do NOT pattern-match on it.
    Message  string
    Repaired bool
}

// Check performs a structural integrity walk. Verifies meta + page
// checksums, B+tree integrity, bitmap consistency, RPL chain, page
// accounting, prefix-compression integrity, keyspace descriptor
// consistency, and set keyspace subpage / nested B+tree integrity.
// Returns issues as an iter.Seq.
//
// Walk failures (I/O errors, unreadable pages) are reported as
// CheckFatal severity and are always the last issue yielded.
//
// Check internally opens a read transaction. The transaction is
// released when the iterator is exhausted OR when the caller
// abandons iteration (a runtime.AddCleanup attached to the iter.Seq
// closure releases the reader slot on GC). Callers iterating to
// completion always see the slot released promptly; callers that
// break early should not assume immediate release.
func (db *DB) Check() iter.Seq[CheckIssue]

type CheckOptions struct {
    // Repair enables offline repair: reclaims leaked pages in the
    // bitmap. Requires exclusive access (no concurrent readers or
    // writers).
    Repair bool

    // CheckIndexes additionally verifies that stored index entries
    // match what the supplied extractors would produce. Re-runs every
    // extractor over every row — O(rows × indexes). Off by default.
    //
    // When true, Indexes below MUST contain an IndexDecl set for each
    // indexed keyspace whose indexes should be verified. Indexed
    // keyspaces absent from the map are skipped for the
    // extractor-equivalence check (structural integrity is still
    // verified) and reported as a CheckWarning with
    // Code = "CheckIndexes.KeyspaceNotSupplied". Mismatched
    // fingerprints for a supplied IndexDecl are reported as
    // CheckError issues with Code = "CheckIndexes.FingerprintDrift"
    // and the offending index name — they do NOT abort the walk and
    // do NOT trigger a rebuild.
    CheckIndexes bool

    // Indexes supplies extractors for the CheckIndexes mode, keyed by
    // keyspace name. Ignored when CheckIndexes is false.
    //
    // Entries in Indexes whose keyspace name does not exist in the
    // database are reported as CheckWarning with
    // Code = "CheckIndexes.KeyspaceNotFound". Entries whose
    // IndexDecl.Name does not match any index registered on the
    // existing keyspace are reported as CheckWarning with
    // Code = "CheckIndexes.IndexNotInRegistry". Both surface common
    // misconfiguration (typos, out-of-date callers) instead of
    // silently skipping.
    Indexes map[string][]*IndexDecl
}

func (db *DB) CheckWithOptions(opts *CheckOptions) iter.Seq[CheckIssue]

// CopyTo creates a consistent copy at the given path. Taken from a
// read-tx snapshot — writers are not blocked. When compact is true,
// the copy is compacted: free pages omitted, B+tree pages written
// sequentially. Inherits source's PageSize, BitmapPages, MaxSize.
// To change file format, re-open the copy and use SetFileFormat.
func (db *DB) CopyTo(path string, compact bool) error

// Compact rebuilds the database file in place. CopyTo(compact=true)
// to a temporary file in the same directory, then atomic rename.
//
// Coordination protocol (Compact is the most invasive single-process
// operation; the caller does not have to "ensure no transactions are
// open" — Compact arranges this itself):
//
//  1. Acquire the cross-process write lock via the flock goroutine,
//     blocking concurrent writers and Checkpoint() for the duration.
//  2. Wait up to Options.CompactDrainTimeout (default 30s) for active
//     read transactions in THIS process to commit/rollback. If any
//     read transaction remains after the timeout, abort with
//     ErrCompactReadersActive (no copy is started, no file rename).
//  3. Open a read snapshot at the current TxnID and run
//     CopyTo(tmpPath, compact=true) — writers in other processes are
//     blocked (the cross-process flock is held), but reads in other
//     processes that already opened a snapshot continue to work
//     against the original inode via their existing mmap; new
//     read-open attempts from other processes during this window
//     succeed against the original inode (open() resolves before
//     rename).
//  4. fsync(tmpPath); atomic rename(tmpPath, originalPath); reopen
//     the file descriptors and mmap; release the cross-process write
//     lock.
//
// Cross-process readers post-rename: pre-rename readers' mmap still
// references the original inode (rename unlinks the directory entry
// but the inode stays alive until the last mapping is released);
// SIGBUS is not possible. Post-rename openers (in other processes)
// observe the new inode via a fresh open() — their UUID check
// matches (Compact preserves UUID), so coordination continues
// normally. There is no observable inconsistency window for
// cross-process readers.
//
// Effects: reclaims leaked pages; defragments file; shrinks to
// minimum size.
//
// Requires enough free disk space for the temporary copy (up to the
// size of the live data) on the SAME filesystem as originalPath
// (otherwise the atomic rename degrades to a copy + delete, breaking
// atomicity).
//
// Fallback when Compact() returns ErrCompactReadersActive: long-lived
// readers cannot be drained in this process. Use
// CopyTo(path, compact=true) instead — it runs from a read snapshot
// without draining in-process readers and produces an offline
// compacted copy you can swap in during scheduled downtime.
func (db *DB) Compact() error
```

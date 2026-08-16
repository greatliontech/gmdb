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
  property=Every keyspace-content keyed-removal API returns
    `ErrNotFound` when the addressed item is absent. The scope is
    `Keyspace.Delete(k)` / `SetKeyspace.Delete(k)` /
    `SetKeyspace.DeleteValue(k, v)` and their typed equivalents
    (`typed.KeyspaceHandle.Delete`, `typed.SetKeyspaceHandle.Delete`,
    `typed.SetKeyspaceHandle.DeleteValue`): all return `ErrNotFound` iff the
    addressed item (the key, or the `(key, value)` pair for the
    value-level variant) does not exist at call time, never
    `nil` for a no-op miss. `Cursor.Delete()` and
    `SetCursor.Delete()` are explicitly **out of scope** for this
    invariant: they are state-bound, not membership-bound, and
    return `ErrCursorUnpositioned` when the cursor is not in the
    Positioned state. A Positioned cursor by construction points
    at a live entry — the gen-counter / `MarkStale` contract in
    `transactions.md §Cursor State Machine` is the membership
    guarantee — so `ErrNotFound` is unreachable there. Bulk-range
    surfaces (`Keyspace.DeleteRange` / `SetKeyspace.DeleteRange`
    and typed equivalents) are also out of scope: they return
    `(0, nil)` for an empty range — "no rows matched" is success
    for a bulk op, not absence. Index-namespace removal
    (`TxIndexes.Drop(keyspace, indexName)`) uses a distinct
    sentinel (`ErrIndexNotFound`) by namespace, not a different
    policy — out of scope here. Keyspace-management
    (`Tx.DeleteKeyspace(name)` returning `ErrNotFound`) is
    consistent with this invariant and documented inline at
    §Keyspace API;
  from=this spec §Invariants (this clause) + signature comments
    in §Keyspace API / §SetKeyspace API + the typed mirror in
    `typed-keyspaces.md`;
  violation=A silent-no-op-on-miss conflates "operation completed
    and changed state" with "operation completed and changed
    nothing" — a caller batching deletes and tracking work-done
    via `err == nil` over-counts effective deletions, breaking
    audit logs and downstream-invalidation triggers.

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

Invariant: kind=clause-explicit;
  property=At no point — including after a crash at any moment
    during `CopyTo` — does the destination path name a file that
    is not a complete, fsynced copy. On every supported
    destination this holds for all non-crash paths; the sole
    crash-window exception is the FAT-class dirent tear scoped in
    the rename rung below. The copy is written to a `<path>.copytmp-*` temp
    file in the destination's directory, fsynced, and only then
    published at path down a per-filesystem atomicity ladder:
    an atomic hard link (also atomic no-clobber: a file appearing
    at path mid-copy fails the publish with the pre-existing file
    untouched); on destinations without hard-link support
    (vfat/exfat, many FUSE mounts), a no-replace rename —
    `renameat2(RENAME_NOREPLACE)` on Linux (still atomic
    no-clobber), degrading to a probe-then-rename where NOREPLACE
    is unsupported and on non-Linux platforms, whose no-clobber is
    BEST-EFFORT (a genuine probe-to-rename TOCTOU window). The
    renamed inode is always complete and fsynced, but the rename
    rung's CRASH-atomicity is the destination filesystem's own
    rename crash-semantics: journaled filesystems keep the
    invariant; FAT-class metadata (no journal) can tear the
    directory entry on power loss during the rename, leaving path
    naming a torn entry — the residual crash exposure on exactly
    the destinations that forced the rename rung. A failed link
    with path
    already naming the copied inode (NFS's link retransmission)
    IS a successful publish. A crash can leave only the
    inert temp, safe to delete once no `CopyTo` is in flight. A
    directory-fsync failure after the publish UNPUBLISHES (removes
    path) before returning the error, so in the live namespace an
    error return means nothing was produced at path (best-effort:
    a remove failure on top of the failed fsync, a crash that
    resurrects the un-fsynced unlink, or a missed
    link-retransmission detection — the quirk probe's own Lstat
    failing transiently on the same flaky mount — can leave the
    file — always a COMPLETE copy, never a partial one);
  from=this spec §Check, CopyTo, Compact (CopyTo destination
    crash-consistency); enforced by `TestCopyToPublishAtomicity`,
    `TestCopyToPublishNeverClobbersLateFile`,
    `TestCopyToDirSyncFailureUnpublishes`,
    `TestCopyToPublishFallbackRenameWhenLinkUnsupported`,
    `TestCopyToPublishFallbackPreservesNoClobber`,
    `TestCopyToPublishNFSRetransmissionQuirk`,
    `TestRenameNoReplaceBestEffort`,
    `TestRenameNoReplaceErrnoRouting`;
  violation=Metas written directly at path can persist before
    the data pages under them (no write-order barrier below the
    final fsync) — a power loss then leaves a torn backup that
    OPENS cleanly and either fails every read with
    `ErrBadPageChecksum` or, with checksums disabled, silently
    serves garbage; the retry's "path exists" failure reinforces
    that the backup is good.

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
    ErrVersionMismatch         = errors.New("gmdb: on-disk format version mismatch")
    ErrReadOnly                = errors.New("gmdb: read-only transaction")
    ErrDatabaseReadOnly        = errors.New("gmdb: database opened read-only")
    ErrTxClosed                = errors.New("gmdb: transaction already committed or rolled back")
    ErrPoisoned                = errors.New("gmdb: database handle is poisoned; Close and re-Open to recover")
    // Commit outcome classes (durability.md §Commit outcome
    // classification): every commit-protocol failure wraps exactly one.
    ErrCommitNotVisible        = errors.New("gmdb: commit failed before publication — the transaction is not the database state")
    ErrCommitVisible           = errors.New("gmdb: commit failed after publication — the transaction IS the database state; do not retry")
    ErrCommitDurabilityUnknown = errors.New("gmdb: commit visible but the final meta fsync failed — stable-storage durability unknown")
    ErrClosed                  = errors.New("gmdb: database is closed")
    ErrCursorUnpositioned      = errors.New("gmdb: cursor not positioned")
    ErrCursorClosed            = errors.New("gmdb: cursor closed")
    ErrChildActive             = errors.New("gmdb: transaction is frozen by an active child transaction")
    ErrKeyspaceKindMismatch    = errors.New("gmdb: keyspace kind does not match existing keyspace")
    ErrKeyspaceReserved        = errors.New("gmdb: keyspace name reserved for engine use")
    ErrValueSizeMismatch       = errors.New("gmdb: value size does not match fixed value size")
    ErrFixedValueSizeMismatch  = errors.New("gmdb: keyspace exists with different FixedValueSize")

    // Indexing.
    ErrIndexExtractorRequired   = errors.New("gmdb: index extractor required for OpenKeyspace")
    ErrIndexUnknown             = errors.New("gmdb: IndexDecl supplied for index not declared in registry")
    ErrIndexFingerprintMismatch = errors.New("gmdb: index fingerprint mismatch — RebuildIndex required")
    ErrIndexUniqueViolation     = errors.New("gmdb: unique index violation")
    // ErrIndexEncoderIDReserved rejects an encoder ID inside the
    // reserved column namespace (gmdb/col/, gmdb/multicol/,
    // gmdb/cover-value/); surfaced through the typed tier's
    // declaration lowering at OpenKeyspace / CreateKeyspace
    // (typed-keyspaces.md §Encoder interface).
    ErrIndexEncoderIDReserved   = errors.New("gmdb: encoder ID is inside the reserved column namespace")
    // ErrIndexKindUnknown rejects an IndexDecl whose Kind this engine
    // version does not implement (indexing.md §Overview —
    // IndexKindComposite is the only kind). Surfaced at OpenKeyspace /
    // CreateKeyspace / RebuildIndex, before any work.
    ErrIndexKindUnknown         = errors.New("gmdb: index kind unknown to this engine version")
    ErrIndexNotUnique           = errors.New("gmdb: Get called on non-unique index")
    ErrIndexExists              = errors.New("gmdb: index already exists")
    ErrIndexNotFound            = errors.New("gmdb: index not found")
    ErrIndexEncoderIDEmpty      = errors.New("gmdb: typed index encoder returned empty ID() — encoder IDs must be unique non-empty strings")
    ErrCoveringTupleMalformed   = errors.New("gmdb: covering tuple malformed")

    // Keyspace lifecycle.
    ErrKeyspaceAlreadyOpen      = errors.New("gmdb: keyspace already opened in this transaction with a different index set")
    ErrKeyspaceClosed           = errors.New("gmdb: keyspace handle is invalid (keyspace deleted in this transaction)")

    // Write batching.
    ErrBatchClosurePanic        = errors.New("gmdb: batch closure panicked")
    ErrBatchClosureGoexit       = errors.New("gmdb: batch closure exited via runtime.Goexit")

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
phase: step-1 data/bitmap/RPL pwrites onward — through step-3 pwrite
of the new meta page and step-4 fdatasync. See `pager-slab.md §Commit
Write Ordering` for the four-step protocol; the publication boundary
is the first pwrite (step 1).

A failure in the **assembly phase** (step 0: tail refund, loose
migration, RPL-segment slab allocation and file extension) does NOT
poison: no pwrite has occurred, `AbortTx` fully restores the pre-tx
in-memory state, and the handle stays usable. The only commit errors
that can originate in assembly are `ErrTxTooLarge` (a defensive
backstop — the commit reserve is bounded at every obligation and
retire event and a pre-commit spill frees everything else, so the
budget cannot be exceeded at `Commit` by in-spec use; see
`pager-slab.md §Slab Budget` INV-COMMIT-HEADROOM) and
`ErrDBFull` (file extension hits `MaxSize`) — step 1+ performs no
allocation — so `Commit` returning either leaves the handle
recoverable and retryable (a fresh, smaller transaction), never
poisoned. This is what lets background
compaction's budget-halving retry an over-large batch (background-maintenance.md §Invariants), and a
single large delete whose RPL append overruns the budget recover
cleanly.

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
there is no in-process repair API in v0 (a future repair surface
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
//
// The context governs cancellation: an already-cancelled / expired ctx
// fails fast before any filesystem work, and cancellation during the
// EEXIST-retry wait aborts promptly (context.Cause(ctx)). ctx does not
// yet abort a syscall already in flight; it is retained on the
// signature so future tracing / timeout / cancellation can be wired
// without a breaking change. opts is passed by value (no nil-pointer
// ambiguity; the zero value applies all defaults).
func Open(ctx context.Context, path string, opts Options) (*DB, error)
```

## Byte Slice Ownership

All `[]byte` slices returned by gmdb (from `Get`, `Cursor.Next`,
`Cursor.Seek`, etc.) are **borrowed references** — they point
into either the mmap, the writer's slab buffer (when reading
own writes in a write txn), or an internal cursor buffer. The
caller does not own them.

**Value slices for inline entries** point directly into the mmap
(for committed pages) or into the writer's slab buffer (for same-
txn modifications). Borrowed; valid until the **transaction
closes** (`Commit()` or `Rollback()`).

**Value slices for overflow entries** are borrowed references
like inline values: an overflow run stores its extent bytes as
ONE contiguous byte range (head-page metadata sits in front, run
pages carry no footers — `page-formats.md §Overflow Page`), so
`Get` / `Cursor` return a single borrowed slice of length
`TotalLen` pointing into the mmap. Valid until the transaction
closes, exactly as for inline values. (Overflow values written in
the SAME write transaction live in per-page slab buffers, not the
mmap — reading one back assembles a freshly-allocated copy whose
lifetime is caller-controlled.) The normative caller rule is
uniform regardless of which case produced the slice: treat every
returned value as borrowed until the transaction closes, and copy
to keep it longer.

**Key slices** may point into the mmap (any key in an
uncompressed leaf — `page-formats.md §Uncompressed Leaf` — or
the restart-point keys of a prefix-compressed leaf), into a
slab buffer, or into the cursor's key reconstruction buffer
(`keyBuf` — the per-cursor backing the `LeafIter`
forward-streaming mode appends into; see `page-formats.md
§Cursor Iteration`). The reconstruction buffer is reused on
each cursor movement. Key slices are valid until the **next
cursor operation** or transaction close, whichever comes
first.

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
   - `RPLHeadPage = 0`, `RPLHeadTxnID = 0`, `RPLTailPage = 0`,
     `RPLEntryCount = 0`
   - `NumFreePages = 0`
   - Durable sub-record self-durable on both: `DurableTxnID =
     AnchoredDurableTxnID = 0`, every `Durable*` field equal to its
     live counterpart (the fsync at step 5 makes genesis the first
     durable epoch — `durability.md §Checkpoints and the durable
     sub-record`)
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
    SyncLazy                       // skips all syncs. On crash, rolls back to the durable epoch (the last fsync point by any handle: a Checkpoint(), a SyncDurable/SyncDataOnly commit, or a clean Close).
)

// Options for opening a database.
type Options struct {
    // PageSize in bytes. Only used when creating a new database.
    // Must be a power of 2 in [4096, 65536]. Default: 4096.
    PageSize uint32

    // DisablePageChecksum turns OFF the XXH3-64 page footers and
    // overflow whole-run digests that are otherwise written and
    // verified on every data page / overflow run. Stored as a flag
    // in the meta page — immutable after creation. The zero value
    // leaves checksums ENABLED (the spec default); opt out only on
    // media with its own end-to-end integrity. Only used when
    // creating; ignored when opening existing.
    DisablePageChecksum bool

    // FileFormat controls database file size bounds and growth.
    // Only used when creating; modify via Tx.SetFileFormat() at runtime.
    FileFormat FileFormat

    // SyncMode controls durability. Default: SyncDurable.
    SyncMode SyncMode

    // NoFullFsync (darwin only) opts the durability barrier down from
    // fcntl(F_FULLFSYNC) to plain fsync(2) — faster, but acknowledged
    // commits can be lost on power loss (durability.md §Platform sync
    // primitives). No effect elsewhere. Default: false.
    NoFullFsync bool

    // MaxReaders is the maximum number of concurrent reader slots.
    // Default: 4096. Only used when creating a new lock file.
    // Type is `uint32` to match the lock-file header field
    // (cross-process.md §Lock File Layout `LockFileHeader.MaxReaders`)
    // and to carry the [1, 65536] bound at the type level (no
    // runtime int→uint32 conversion needed).
    MaxReaders uint32

    // MaxTxBufferBytes is the per-write-transaction slab SPILL
    // THRESHOLD, not a transaction-size cap: past it, modified pages
    // are written out to their allocated file locations at operation
    // boundaries and their buffers freed, so transactions of any
    // size commit within bounded steady-state memory
    // (pager-slab.md §Slab Budget; TxStats.SpilledPages counts the
    // early write-outs for tuning). ErrTxTooLarge remains only for
    // the commit RESERVE — the slab Commit itself must allocate,
    // which cannot spill: a retired-page log (RPL) that alone
    // outgrows the threshold (a huge DeleteRange or compaction
    // pass), or a descriptor-flush obligation that cannot fit. A
    // transaction that saw ErrTxTooLarge from an operation can
    // still Commit the work it applied (INV-COMMIT-HEADROOM).
    //
    // Sizing guide: the threshold trades memory for early pwrites —
    // below it, a transaction's working set stays in memory and is
    // written once at commit; past it, superseded and excess pages
    // are written as they retire. Byte slices borrowed from the
    // transaction (Get results, cursor reads) stay valid regardless.
    // Default: 256 MiB.
    MaxTxBufferBytes int

    // RestartGroupTarget is the engine-wide default for the leaf
    // restart-group target (the maximum entries per group on
    // compressed leaves). Per-keyspace overrides via
    // Tx.SetKeyspaceConfig(). Bounded to [0, 255]: 0 ⇒ engine
    // default (6); 1 ⇒ uncompressed leaf variant
    // (page-formats.md §Leaf Page §Uncompressed Leaf), the
    // operational choice for keyspaces whose keys don't share
    // prefixes (random, hash, unique-id); [2, 255] ⇒ compressed
    // leaves with that target. Open() rejects values > 255 with
    // ErrInvalidOptions — the compressed-leaf restart-table
    // Count field is uint8, so 255 is the hard physical cap.
    // Default: 6.
    RestartGroupTarget int

    // LeafLayout and BranchLayout are the engine-wide default node
    // layout variants for keyspaces whose descriptor declares the
    // engine default (keyspaces.md §Keyspace Descriptor NodeLayouts;
    // page-formats.md §Leaf Page / §Branch Page). Zero values take
    // the engine defaults (segregated for both). Per-OPEN, not
    // persisted (unlike DisablePageChecksum): the opening process's
    // value resolves descriptor-0 keyspaces at page-build time, the
    // same semantics as Options.RestartGroupTarget — per-page
    // type-byte dispatch makes differing defaults across opens
    // yield mixed-layout pages, never misreads. Per-keyspace
    // overrides via CreateKeyspace options / Tx.SetKeyspaceConfig().
    // Open() rejects unknown values with ErrInvalidOptions.
    LeafLayout LeafLayout     // LeafLayoutDefault | LeafLayoutInterleaved | LeafLayoutSegregated
    BranchLayout BranchLayout // BranchLayoutDefault | BranchLayoutPlain | BranchLayoutSegregated

    // MergeThreshold is the B+tree page fill percentage that doubles
    // as the post-deletion merge trigger AND the maintained non-root
    // fill floor. Range: 1-50. Default: 25.
    //
    // Trigger: a page that drops below this percentage of ContentEnd
    // after Delete / DeleteRange is merged with (or redistributed
    // against) an adjacent sibling. Floor: after Delete / DeleteRange
    // returns successfully, every non-root page reachable from the
    // new root has fill >= MergeThreshold% of ContentEnd — see
    // range-delete.md §Invariants for the post-merge re-rebalance
    // loop and the cousin-cascade thread that maintain the floor.
    // The root is exempt.
    MergeThreshold int

    // LaggingReader is called when a long-lived reader is blocking
    // RPL reclamation during page allocation. If nil, pageAlloc()
    // falls through to file extension when reclamation is blocked.
    LaggingReader func(info LaggingReaderInfo) LaggingReaderAction

    // MaxBatchSize is the maximum number of Batch() calls collected
    // before executing in one transaction. Default: 1000.
    MaxBatchSize int

    // MaxBatchDelay is the maximum time to wait for additional
    // Batch() calls before executing the current batch. The zero value
    // takes the 10ms default; for minimal coalescing set MaxBatchSize=1.
    // Default: 10ms.
    MaxBatchDelay time.Duration

    // StaleTimeout for cross-PID-namespace stale detection via
    // heartbeats. Default: 10s.
    StaleTimeout time.Duration

    // HeartbeatInterval is the refresh cadence for both heartbeat
    // fields: the flock goroutine refreshes `WriterHeartbeat` at this
    // interval while it holds `LOCK_EX`, and the heartbeat goroutine
    // refreshes the `Heartbeat` field of every active reader slot.
    // Must be significantly less than `StaleTimeout` for scheduling
    // jitter. Default: 1s. See cross-process.md §Heartbeat Goroutine.
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

    // ReadOnly opens the database in read-only mode: the writer pager
    // path is not initialised, no background maintenance runs, and the
    // write entry points (Begin / Update / Batch / Compact /
    // Checkpoint) return ErrDatabaseReadOnly. The data mmap is always
    // PROT_READ regardless.
    //
    // Cross-process safety. A read-only handle still participates in
    // the reader-table protocol so a concurrent cross-process WRITER
    // cannot reclaim pages out from under it: when the lock file can
    // be opened read-write, Open starts the heartbeat goroutine and
    // every read transaction acquires a reader slot exactly as a
    // read-write handle does — only the writer flock-grant goroutine
    // is skipped (this handle never takes LOCK_EX). On truly read-only
    // media (the lock file cannot be opened read-write), Open falls
    // back to lock-free snapshot reads and logs a warning; in that
    // fallback a concurrent writer on shared storage could reclaim
    // pages under an in-flight reader (torn reads) — but a read-only
    // medium normally precludes any writer, so the common case is safe.
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

// Close tears the handle down after draining in-flight transactions.
// On a writable, non-poisoned handle it first performs the Checkpoint
// sequence under the write grant (durability.md §Clean shutdown), so
// a clean Close never loses acknowledged commits regardless of
// SyncMode; the checkpoint's failure surfaces as Close's error, and a
// poisoned handle skips the checkpoint (re-Open converges). Acquiring
// the grant can block behind a peer's writer or Compact — Close waits
// (it takes no context); callers needing bounded shutdown should
// Checkpoint(ctx) first and accept the crash-equivalent rollback on
// timeout.
func (db *DB) Close() error

// Checkpoint flushes all outstanding writes to stable storage and
// advances the durable epoch to the active meta's TxnID (crash
// recovery rolls back no further than this point — durability.md
// §Checkpoints and the durable sub-record). Primarily for SyncLazy;
// in SyncDataOnly it additionally anchors the latest epoch assertion
// (durability.md §Anchoring), and in SyncDurable it is a no-op
// beyond an extra fsync (commits already sync end-to-end).
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
func (db *DB) View(ctx context.Context, fn func(rtx *ReadTx) error) error

// Update executes a read-write transaction.
func (db *DB) Update(ctx context.Context, fn func(tx *Tx) error) error

// Panic safety (Update / View): unlike Batch, these wrappers do NOT recover
// a panic in fn — it propagates to the caller. They DO, however, release the
// transaction's resources before the panic unwinds: Update rolls back (freeing
// the cross-process write grant + pager tx state) and View releases its
// reader-table slot. A panic recovered higher up the stack therefore never
// leaks the grant (which would block all writers across all processes on
// AcquireWriter) or a reader slot (which pins RPL reclamation and counts toward
// ErrReadersFull) until GC. The release is synchronous, via a deferred close.

// Batch submits a write operation to be batched with other concurrent
// callers into a single transaction. The context governs the wait for
// batch inclusion and one pre-dispatch check: once accepted, the
// caller blocks until the batch resolves (ctx cannot unblock the
// wait); a ctx that fires before dispatch skips the closure — it runs
// AT MOST once — and the caller receives context.Cause(ctx); once
// dispatched, the outcome is reported truthfully regardless of ctx.
// Each closure runs in its own child transaction. See
// `transactions.md §Write Batching`.
//
// A closure that returns an error has its child rolled back and that
// error returned to its caller; siblings are unaffected. A closure that
// panics is recovered: its child is rolled back and the caller receives
// ErrBatchClosurePanic wrapping the panic value, while sibling closures
// still run. A closure that exits via runtime.Goexit (t.FailNow and
// friends) is contained the same way; its caller receives
// ErrBatchClosureGoexit. If the parent batch commit fails, every
// caller whose closure succeeded receives the commit error.
//
// The closure MUST NOT call Commit() or Rollback() on the supplied
// *Tx — the batch coordinator owns child-transaction lifecycle. A
// closure that calls either causes the coordinator's subsequent
// child-commit-or-rollback to error with ErrTxClosed, which
// propagates to the caller as the closure's result — note a
// self-COMMITTED child's write still lands if the parent batch
// commits (the self-commit already merged it); the error reports the
// contract violation, not the write's outcome. (A closure may open
// its own nested BeginChild, but must resolve it before returning, or
// the caller receives ErrChildActive.)
func (db *DB) Batch(ctx context.Context, fn func(tx *Tx) error) error

// Begin starts a write transaction. The context governs the wait
// for the cross-process write lock; once Begin returns a *Tx the
// context is not stored. For read transactions use BeginRead.
//
// Read and write transactions are distinct types — write goes through
// Begin (returning *Tx); read goes through BeginRead (returning
// *ReadTx) — so the type system rejects write methods on a read
// snapshot at compile time. (There is no read-via-Begin flag; the
// read-tx surface lives on db.BeginRead / db.View / *ReadTx.)
func (db *DB) Begin(ctx context.Context) (*Tx, error)

// BeginRead opens a snapshot read transaction. The returned *ReadTx
// pins the active meta's TxnID via a reader-table slot; callers
// MUST eventually call Commit or Rollback (both equivalent —
// release the slot) so RPL reclamation can advance past the
// snapshot.
//
// Errors:
//   - context.Cause(ctx) if ctx fires before slot acquisition.
//   - ErrClosed if the DB's coordination goroutines have shut down.
//   - ErrReadersFull if the reader table is at capacity and ctx has
//     no deadline. With a deadline, the call retries until a slot
//     frees or the deadline fires (context.DeadlineExceeded).
func (db *DB) BeginRead(ctx context.Context) (*ReadTx, error)

// Tx is a write transaction.
type Tx struct { ... }

// Commit publishes the transaction's changes atomically via the meta
// swap.
//
// Descriptor flush is deferred to Commit (the deferred-flush design).
// Same-tx Keyspace.Put / Keyspace.Delete / Cursor.Delete /
// SetKeyspaceConfig / CreateKeyspace* / DeleteKeyspace mutate the
// keyspace descriptors only in memory; the on-disk keyspace B+tree
// is updated by Commit before pager-Commit's meta swap. Consequence
// for the error surface: ErrTxTooLarge from a slab-budget exhaustion
// caused by the keyspace-B+tree CoW path surfaces from Commit
// itself, not from the originating per-op method. A Put / Delete
// returning nil is a guarantee that the in-memory descriptor was
// updated, NOT that the keyspace B+tree has been touched on disk;
// the on-disk effect lands iff Commit returns nil. Rollback drops
// the in-memory state and leaves no on-disk trace.
func (tx *Tx) Commit() error
func (tx *Tx) Rollback() error

// ReadTx is a snapshot read transaction. Distinct type from *Tx so
// the type system rejects write methods on read snapshots at
// compile time. See db.BeginRead / db.View.
type ReadTx struct { ... }

// ReadTx reads keyspace data through its pinned snapshot — gmdb's
// concurrent-read path (transactions.md §Read Transaction). The read
// handles it returns observe a consistent, immutable view that a
// concurrent committed write never changes, and the reader never blocks
// the single writer (the only contention is reader-slot acquisition at
// BeginRead). Reads are backed by a read-only *Tx (writable=false) over
// the snapshot's read-only mmap, so every *Keyspace / *SetKeyspace read
// op works unchanged and every mutator returns ErrReadOnly.
//
// Raw page bytes and the decoded internal meta are NOT exposed: page
// access is the cursor / B+tree layer's concern (a public Page(id)
// invites a HighWaterMark-out-of-range SIGBUS — mmap-strategy.md
// §Sparse Reservation), and pager.Meta is an internal storage struct.
// The snapshot identity callers need is the curated TxnID (uint64);
// DB.Stats (§Statistics) covers DB-level numbers.
//
// A ReadTx serves one goroutine: it is not safe for concurrent use
// by multiple goroutines. Concurrent reads are N ReadTxs — one
// BeginRead per goroutine — not N goroutines on one snapshot.
//
// The typed tier reads through a ReadTx via typed.ReadOpener — an
// interface both *Tx and *ReadTx satisfy (typed-keyspaces.md
// §Snapshot reads).
func (rtx *ReadTx) OpenKeyspaceReadOnly(name string) (*Keyspace, error)
func (rtx *ReadTx) OpenSetKeyspaceReadOnly(name string) (*SetKeyspace, error)
func (rtx *ReadTx) ListKeyspaces() ([]string, error)
func (rtx *ReadTx) TxnID() uint64

// Commit and Rollback are equivalent for ReadTx — both release the
// reader slot and close the snapshot. The pair exists for symmetry
// with the write-tx surface and the standard `defer rtx.Rollback()`
// pattern in caller code.
func (rtx *ReadTx) Commit() error
func (rtx *ReadTx) Rollback() error

// BeginChild creates a child transaction within the current write
// transaction. Children can be committed (merged into parent) or
// rolled back (discarded) independently, and may nest to arbitrary
// depth. Only valid on a write txn.
//
// While the child — or any descendant — is open, the parent and every
// ancestor are FROZEN: data ops, Commit, and a second BeginChild on
// them return ErrChildActive until the child resolves. Rollback is
// the exception — it cascade-rolls-back the open descendant chain
// deepest-first and then the transaction itself (transactions.md
// §Nested Transactions), so a dropped child handle can never strand
// the write grant.
// Handles opened on the child are valid only for the child's lifetime
// (ErrTxClosed after it resolves); continue through a parent handle.
// See transactions.md §Nested Transactions.
func (tx *Tx) BeginChild() (*Tx, error)

// SetFileFormat updates the file format. MaxSize is immutable and
// cannot be changed; returns an error if FileFormat.Upper differs.
// Only valid on a write transaction.
func (tx *Tx) SetFileFormat(f FileFormat) error
```

## Change Notification

Blocking change observation across processes, carried by the lock
file's notification region (`cross-process.md §Lock File Layout,
notification region`). Versions are opaque uint64 tokens: every
committed-visible commit — from any process — produces a version
greater than every version observed before it, and tokens from all
three methods are mutually comparable across handles sharing a
notification source (the lock file; or, for lock-free read-only
fallback handles, the data file's committed state — the two sources
can diverge after a Compact, so tokens must not be compared across
the two handle modes). They are NOT transaction IDs and carry no
meaning beyond ordering; persisting them across process restarts is
meaningful only for ordering against other persisted tokens of the
same database and source.

```go
// Version returns the database's current commit version. Use it as
// the `from` argument of the waits below. Errors include ErrClosed
// on a closed handle and ErrPoisoned on a poisoned one (same for the
// waits); the lock-free fallback can additionally surface data-file
// read errors (its version source is a meta pread).
//
// On the read-only lock-free fallback (no lock file; see
// Options.ReadOnly) the version derives from the data file's
// committed state.
func (db *DB) Version() (uint64, error)

// WaitVersion blocks until the database's commit version exceeds
// from, and returns the observed version. Returns ctx.Err() on
// context cancellation and ErrClosed if the handle is closed while
// waiting (Close does not block on in-flight waits; it ends them).
// A successful return always means version > from, and the commit
// that produced the returned version is observed by any read
// transaction opened afterwards — on any handle able to open one.
// (The one handle that may not be: a commit that failed with a
// classified visible/durability-unknown outcome wakes waiters, but
// poisons its OWN handle — reads there fail loudly with ErrPoisoned
// rather than observing anything; peers and a re-Open observe the
// commit.)
//
// Wake latency: sub-millisecond on Linux (futex); single-digit
// milliseconds elsewhere (adaptive poll). On the read-only
// lock-free fallback the wait polls the data file's committed meta.
func (db *DB) WaitVersion(ctx context.Context, from uint64) (uint64, error)

// WaitKeyspaceVersion is WaitVersion scoped to commits that touched
// the named keyspace: data writes, creation, configuration changes,
// deletion. The keyspace need not exist — waiting for its creation
// is valid. Scoping is by name hash over a fixed slot array, so an
// unrelated keyspace's commit can end the wait spuriously (hash
// collision); a successful return still guarantees version > from.
// Callers re-check the data they care about — the wait is a wake
// primitive, not a change description. A no-op commit against the
// keyspace (a presence-gated verb that mutated nothing) does not
// wake scoped waiters.
//
// On the read-only lock-free fallback, keyspace waits degrade to
// global waits (every commit wakes them — spurious by contract).
func (db *DB) WaitKeyspaceVersion(ctx context.Context, name string, from uint64) (uint64, error)
```

Version monotonicity survives a lock-file recreation on a database
that has never been compacted (the region is re-seeded from the
committed transaction ID). After a `Compact`, versions remain
monotonic as long as the lock file — transient coordination state —
survives; deleting it after a compaction may restart versions lower.
Waits are delivery-guaranteed for commits that report success or a
classified visible/durability-unknown outcome (`ErrCommitVisible`,
`ErrCommitDurabilityUnknown`); an unclassified commit failure's
changes wake waiters at the next successful commit.

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
    // FixedValueSize must be <= the inline threshold T
    // (limits.md §Maximum Key Size); creation with a larger
    // value returns ErrInvalidOptions — fixed-stride storage
    // cannot hold overflow-key members.
    FixedValueSize int
}

func (tx *Tx) OpenSetKeyspace(name string, indexes ...*IndexDecl) (*SetKeyspace, error)
func (tx *Tx) OpenSetKeyspaceReadOnly(name string) (*SetKeyspace, error)
func (tx *Tx) CreateSetKeyspace(name string, opts *SetKeyspaceOptions, indexes ...*IndexDecl) (*SetKeyspace, error)

// CreateSetKeyspaceIfNotExists opens the keyspace if it exists (with
// matching Kind=1 + matching FixedValueSize required) or creates it
// with the supplied opts. Returns ErrKeyspaceKindMismatch if the
// existing keyspace is a Kind=0 Keyspace. Returns
// ErrFixedValueSizeMismatch if the existing keyspace's
// FixedValueSize disagrees with opts.FixedValueSize — the
// FixedValueSize is immutable after creation (keyspaces.md
// invariant #5), so the API cannot silently coerce the caller's
// opts to the existing value without misleading them about the
// storage layout. A nil opts is treated as FixedValueSize=0
// (variable values); the same equality check applies. Returns the
// existing index-matching errors when indexes are supplied.
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
// Errors: ErrKeyEmpty if name is nil or empty.
// ErrNotFound if the keyspace does not exist.
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

// SetKeyspaceConfig updates mutable per-keyspace settings:
// RestartGroupTarget and the node layout declarations. Returns
// ErrInvalidOptions for out-of-range values (RestartGroupTarget >
// 255 — the compressed-leaf restart-table Count field is uint8,
// see page-formats.md §Compressed Leaf — or an unknown layout
// value). Every KeyspaceConfig field uses its zero value as the
// "leave unchanged" sentinel (distinct from the descriptor's 0 =
// engine-default semantic). Layout changes are builder hints:
// existing pages keep their on-disk variant and migrate when next
// split, merged, or rebuilt (keyspaces.md §Per-Keyspace
// Configuration). Returns ErrNotFound when name does not resolve
// to an existing keyspace (matching Tx.DeleteKeyspace and the
// Delete-on-miss invariant family). Only valid on a write
// transaction.
func (tx *Tx) SetKeyspaceConfig(name string, cfg KeyspaceConfig) error

type KeyspaceConfig struct {
    RestartGroupTarget uint16       // 0 = leave unchanged; otherwise [1, 255]
    LeafLayout         LeafLayout   // zero = leave unchanged
    BranchLayout       BranchLayout // zero = leave unchanged
}

// TxIndexes is the index-administration surface of a write
// transaction, returned by Tx.Indexes. Its operations address a
// keyspace by NAME rather than through an opened *Keyspace handle:
// Rebuild is the recovery path after ErrIndexFingerprintMismatch,
// invoked while OpenKeyspace is still FAILING for that keyspace — so
// no writable *Keyspace handle exists at the point of call
// (indexing.md §Rebuild, §Recovery pattern after
// ErrIndexFingerprintMismatch). Drop shares the surface for symmetry.
// TxIndexes carries the parent transaction's read-only / closed state
// (Rebuild and Drop return ErrReadOnly / ErrTxClosed exactly as the
// other write APIs).
type TxIndexes struct { /* unexported: bound to the parent *Tx */ }

// Indexes returns the index-administration surface for this write
// transaction. See TxIndexes.
func (tx *Tx) Indexes() TxIndexes

// Rebuild drops and re-populates the named index using the
// supplied IndexDecl (whose Name must match an existing registry
// entry on the keyspace). Bypasses the open-time fingerprint check;
// this is the recovery path after ErrIndexFingerprintMismatch.
// Blocking — runs inside the current write transaction. See the
// `indexing.md §Rebuild` section for the recovery pattern.
//
// Handle invalidation (indexing.md §Handle Invalidation): every
// in-flight *IndexHandle iter on this name surfaces ErrCursorStale on
// the next yield. The handle stays usable — a re-iterate after
// the rebuild opens a fresh cursor on the new pinned.root.
//
// Errors:
//   - ErrKeyEmpty if keyspace is nil or empty, or decl.Name is empty.
//   - ErrIndexExtractorRequired if decl.Extract is nil
//     (`indexing.md §Rebuild`).
//   - ErrNotFound if the keyspace does not exist (keyspace-management
//     dimension; matches Tx.DeleteKeyspace and Tx.SetKeyspaceConfig).
//   - ErrIndexNotFound if the keyspace exists but decl.Name does not
//     match any registry entry on the keyspace (index-management
//     dimension; matches TxIndexes.Drop). Distinct sentinel from
//     ErrNotFound so callers writing the recovery loop (see
//     `indexing.md §Recovery pattern after ErrIndexFingerprintMismatch`)
//     can dispatch between keyspace-missing and index-name-missing
//     without inspecting Tx state.
//   - ErrKeyspaceReserved if the supplied keyspace name resolves
//     to an engine-internal Kind=2 entry.
//   - ErrIndexKindUnknown if decl.Kind is not a kind this engine
//     version implements (indexing.md §Overview).
//   - ErrIndexUniqueViolation if the rebuild's extractor produces
//     duplicate keys for a unique index.
//   - ErrTxTooLarge if the rebuilt index exceeds MaxTxBufferBytes
//     (caller should use BulkLoad or chunk the rebuild).
//   - ErrReadOnly on a read-only transaction; ErrTxClosed on a
//     closed transaction.
//
// The error split is deliberate: ErrNotFound for a missing
// keyspace, ErrIndexNotFound for a missing decl.Name — two
// different management dimensions.
func (ix TxIndexes) Rebuild(keyspace string, decl *IndexDecl) error

// Drop removes the named index entirely. Retires the index's
// internal Kind=2 keyspace pages and the registry entry; if the
// dropped index was the keyspace's last, resets
// desc.IndexRegistryRoot to 0 and retires the registry sub-tree
// (per `keyspaces.md` invariant #7 entailed).
//
// Handle invalidation (indexing.md §Handle Invalidation): every
// previously-handed-out *IndexHandle for this (keyspace, name)
// pair becomes dead — subsequent Lookup/LookupKeys/Range/Prefix/
// Get/Stats return ErrIndexNotFound. An in-flight iter at the
// moment of the drop surfaces ErrCursorStale on the next yield,
// after which the handle stays permanently dead within the
// transaction.
//
// Errors:
//   - ErrKeyEmpty if keyspace is nil or empty, or indexName is empty.
//   - ErrNotFound if the keyspace does not exist.
//   - ErrIndexNotFound if the keyspace exists but indexName does
//     not match any registry entry.
//   - ErrKeyspaceReserved if the keyspace name resolves to a
//     Kind=2 entry.
//   - ErrReadOnly / ErrTxClosed as for other write APIs.
func (ix TxIndexes) Drop(keyspace, indexName string) error

// Keyspace is a handle to a named single-value keyspace.
type Keyspace struct { ... }

func (ks *Keyspace) Get(key []byte) ([]byte, error)
func (ks *Keyspace) Put(key, value []byte) error

// Insert stores (key, value) only when key is ABSENT; on a present
// key it returns ErrKeyExists and mutates NOTHING — no page write,
// no descriptor change, no cursor invalidation, no index
// maintenance (the presence gate fires before any of them). Put
// remains the upsert. The un-indexed path is a single descent
// (btree.InsertIfAbsent's no-op-on-present); the indexed path
// gates on the old-value read it performs anyway.
func (ks *Keyspace) Insert(key, value []byte) error

// Replace stores (key, value) only when key is PRESENT; on an
// absent key it returns ErrNotFound and mutates NOTHING — the
// update-only dual of Insert, with the same no-op purity and the
// same single-descent un-indexed path (btree.ReplaceIfPresent).
//
// SetKeyspace deliberately has neither verb: its Put already
// reports `added` (insert semantics without a rejection), and a
// member-level Replace has no meaning in a set (the member IS the
// value).
func (ks *Keyspace) Replace(key, value []byte) error

// Delete returns ErrNotFound when the key does not exist (per
// §Invariants — keyed-removal returns ErrNotFound on miss).
func (ks *Keyspace) Delete(key []byte) error

// DeleteRange deletes every key k with start <= k < end. Returns
// the count of entries deleted; (0, nil) for an empty range
// (start == end, start > end, or no matching keys) — bulk operations
// report "rows affected", not membership.
//
// Boundary semantics:
//   - nil = open-boundary sentinel. nil start = "from the beginning";
//     nil end = "through the last key"; (nil, nil) = every key.
//   - Non-nil zero-length ([]byte{}) is rejected with ErrKeyEmpty,
//     consistent with every other name-taking API per api-surface.md
//     §Invariants empty-key clause. The asymmetric semantic between
//     nil and []byte{} is intentional: nil expresses "open"; the
//     zero-length byte slice is an invalid key.
//
// **Atomicity contract.** Dispatches by index presence (the
// indexed-keyspace fallback per range-delete.md §Indexed-keyspace
// fallback):
//   - **Un-indexed**: btree.DeleteRange (the single-descent atomic
//     three-phase walker). **Atomic on error**: returns (0, err)
//     with no observable mutations — desc.Root, desc.Count, and
//     the dirty/cursor-stale state are touched only on the
//     success return, so successive same-tx reads after a failed
//     call observe the pre-call state; tx-level Rollback restores
//     the on-disk pager bitmap per pager-slab.md.
//   - **Indexed**: per-row cursor walk + Cursor.Delete per row
//     (atomic index maintenance clears index entries
//     before removing the row). **Per-row atomic on error**:
//     returns (deleted_so_far, err); iterations 0..i-1 have
//     completed and are in-memory visible; the failing iteration
//     and remainder are untouched. Each successful per-row delete
//     satisfies the keyed-removal invariants and
//     the atomic-Put/Delete invariant individually; the
//     in-memory + on-disk state is consistent-but-partial. The
//     only safe recovery is Tx.Rollback() (which restores via
//     the pager bitmap snapshot).
func (ks *Keyspace) DeleteRange(start, end []byte) (uint64, error)

// DeletePrefix deletes every key beginning with prefix, returning the
// count deleted. Pure composition over DeleteRange — the range is
// [prefix, prefixSuccessor(prefix)), an all-0xFF prefix's successor
// being the open upper bound — so semantics, atomicity, the indexed
// fallback, and the error surface are DeleteRange's. A nil or empty
// prefix deletes every key (the Prefix iterator's nil-yields-all
// convention).
func (ks *Keyspace) DeletePrefix(prefix []byte) (uint64, error)
func (ks *Keyspace) NextSequence() (uint64, error)
func (ks *Keyspace) Cursor() *Cursor

// Index returns a handle for querying the named index on this
// keyspace. Returns ErrIndexNotFound if no index with this name is
// registered. See `indexing.md §Lookup API` for query semantics.
func (ks *Keyspace) Index(name string) (*IndexHandle, error)

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

// SeekLE positions at the LARGEST key <= target (the floor);
// SeekLT at the largest key strictly < target. Both return
// (nil, nil) with Err() == nil when no qualifying key exists —
// the backward duals of SeekGE, composed over the same descent
// machinery (a miss lands on the successor and steps back; an
// all-keys-smaller tree lands on Last).
func (c *Cursor) SeekLE(target []byte) (key, value []byte)
func (c *Cursor) SeekLT(target []byte) (key, value []byte)
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

// Close releases the cursor before the transaction ends:
// unregisters it from the keyspace's staleness tracking and makes
// every subsequent operation surface ErrCursorClosed. Terminal and
// idempotent; an earlier sticky error is preserved. Optional — an
// unclosed cursor stays valid for the tx lifetime. See
// `transactions.md §Cursor State Machine` (explicit cursor
// release). SetCursor and typed.Cursor carry the same method.
func (c *Cursor) Close()

func (c *Cursor) Err() error
```

### SetKeyspace API

```go
// SetKeyspace handle to a named set keyspace.
type SetKeyspace struct { ... }

func (ks *SetKeyspace) Has(key []byte) (bool, error)
func (ks *SetKeyspace) HasValue(key, value []byte) (bool, error)

// Put inserts value into the key's sorted set. added reports whether
// the set actually grew (false iff (key, value) was already present —
// the call is a no-op in that case). The membership probe is already
// paid by the insert path (the B+tree / subpage layer must locate the
// insertion point to detect the duplicate-no-op case), so surfacing
// the bool is a no-cost API enhancement that collapses the
// Put + HasValue pattern callers would otherwise need for pub/sub
// broadcasts, ref-counted indexes, idempotent retries — all of which
// need to know "did this call cause the set to grow" without the
// TOCTOU window of HasValue-then-Put. (User-locked decision;
// the typed mirror typed.SetKeyspaceHandle.Put propagates the bool.)
func (ks *SetKeyspace) Put(key, value []byte) (added bool, err error)

// Delete returns ErrNotFound when the key does not exist (per
// §Invariants — keyed-removal returns ErrNotFound on miss).
func (ks *SetKeyspace) Delete(key []byte) error

// DeleteValue returns ErrNotFound when the (key, value) pair
// does not exist (per §Invariants — value-level removal).
func (ks *SetKeyspace) DeleteValue(key, value []byte) error

func (ks *SetKeyspace) CountValues(key []byte) (uint64, error)

// DeleteRange deletes every (key, value) pair whose KEY falls in
// [start, end) and returns the count of VALUES deleted (NOT keys
// — set-keyspace.md §Invariants entailed value-count accounting). Returns (0, nil) for an empty
// range. Boundary semantics match Keyspace.DeleteRange: nil =
// open-boundary; non-nil zero-length is rejected with
// ErrKeyEmpty.
//
// **Atomicity contract (mirrors Keyspace.DeleteRange's
// indexed/un-indexed split).** Dispatches by index presence:
//   - **Un-indexed**: btree.DeleteRange (the single-descent atomic
//     three-phase walker) with a SetKeyspace-aware per-cell free
//     callback that handles subpage / nested-tree / overflow
//     cells. **Atomic on error**: returns (0, err) with no
//     observable mutations — tx-level Rollback restores via the
//     pager bitmap snapshot per pager-slab.md.
//   - **Indexed**: per-row cursor walk + ks.Delete(k) per row
//     (the per-(setKey, setValue) index maintenance
//     clears index entries via the extractor). **Per-row atomic
//     on error**: returns
//     (deleted_so_far, err); iterations 0..i-1 have completed
//     and are in-memory visible; the failing iteration and
//     remainder are untouched. Each successful per-row delete
//     satisfies set-keyspace.md §Invariants value-count accounting; the in-memory + on-disk state
//     is consistent-but-partial. The only safe recovery is
//     Tx.Rollback() (which restores via the pager bitmap
//     snapshot).
func (ks *SetKeyspace) DeleteRange(start, end []byte) (uint64, error)

// DeletePrefix deletes every key (and all its members) beginning with
// prefix, returning the total member-value count deleted — the same
// composition over DeleteRange as the Kind=0 form, with SetKeyspace
// DeleteRange's semantics (range-delete.md §Set Keyspace Range
// Delete). A nil or empty prefix deletes every key.
func (ks *SetKeyspace) DeletePrefix(prefix []byte) (uint64, error)
func (ks *SetKeyspace) NextSequence() (uint64, error)
func (ks *SetKeyspace) Cursor() *SetCursor
func (ks *SetKeyspace) Index(name string) (*IndexHandle, error)
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

// Key-level floor duals: position at (largest-key-<=-target,
// firstValueOf thatKey) / strictly-< variant. (nil, nil) when no
// qualifying key exists.
func (c *SetCursor) SeekLE(target []byte) (key, value []byte)
func (c *SetCursor) SeekLT(target []byte) (key, value []byte)
func (c *SetCursor) Current() (key, value []byte)
func (c *SetCursor) Delete() error
// Close — same semantics as Cursor.Close (explicit cursor release).
func (c *SetCursor) Close()
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

// Err is the post-iteration error check: it reports how the most
// recent All / Range / Prefix sequence on this handle ended.
func (ks *Keyspace) Err() error
func (ks *SetKeyspace) Err() error
```

**Post-iteration error check.** The `Seq2` shape has no error
channel, so a mid-iteration cursor error ENDS the sequence and the
handle's `Err()` reports it after the loop — a truncated iteration
is distinguishable from a smaller keyspace. The contract mirrors
`IndexHandle.Err`:

- `Err()` describes the LAST sequence's iteration: it resets when a
  sequence's iteration starts (not at construction), and is nil
  after every clean end — exhaustion, a `Range` end bound, a
  `Prefix` mismatch, or a caller `break`. With nested or
  `iter.Pull`-interleaved sequences on one handle, the slot holds
  the outcome of the sequence that ENDED last.
- An error-truncated sequence stops at the failing row without
  yielding it: an overflow-value assembly failure (e.g.
  `ErrBadPageChecksum`) must NOT surface as a phantom `(key, nil)`
  yield. `Err()` carries the fault (the same wraps `Cursor.Err`
  produces).
- A loop-body mutation that ends the sequence via the stale
  contract leaves `Err() == ErrCursorStale` — an intentional
  mutate-and-restart caller sees the truncation signal and
  re-iterates.
- Broader handle truths win over the sticky per-sequence error, in
  `IndexHandle.Err`'s order: a DEAD handle reports
  `ErrKeyspaceClosed`, a closed transaction `ErrTxClosed`, a closed
  DB `ErrClosed`, a parent frozen by an active child
  `ErrChildActive`. The cascade's `ErrChildActive` leg is transient
  — it clears when the child resolves — but a sequence that was
  itself truncated by the freeze records `ErrChildActive` in the
  per-sequence slot, reported until the next sequence resets it.
- The handle serves one goroutine, like every handle surface;
  concurrent iteration on one handle races on `Err`.

The typed layer (`typed.KeyspaceHandle.Err` /
`typed.SetKeyspaceHandle.Err`) layers its decode/encode errors on
top of this check (typed-keyspaces.md). `gmdb/query` does not
consume this surface: it drives cursors and index handles directly
and performs its own equivalent post-iteration checks, folded into
`Query.Err`.
(Pinned by `TestIteratorErrNilOnCleanEnds`,
`TestIteratorErrReportsLoopBodyMutationStale`,
`TestIteratorErrReportsBadPageChecksum`,
`TestSetIteratorErrReportsBadPageChecksum`,
`TestIteratorErrNoPhantomPairOnCorruptOverflowValue`,
`TestIteratorErrReportsChildFreezeTruncation`, and
`TestIteratorErrHandleCascade`, with SetKeyspace and typed mirrors.)

Constructing an iterator on a handle whose state forbids every
operation — a parent frozen by an active child (`ErrChildActive`),
a closed transaction (`ErrTxClosed`), a closed DB (`ErrClosed`), or
a DEAD keyspace handle (`ErrKeyspaceClosed`, the keyspace deleted
in this transaction) — PANICS at the `All`/`Range`/`Prefix` call:
these are programmer errors, and a silently empty sequence would be
indistinguishable from no data (the Seq2 shape has no error
channel). The typed layer's `All`/`Range`/`Prefix` run the same
guard eagerly, so the panic fires at the typed call too, not at
loop start. A state change AFTER construction (mid-loop close or
freeze) still ends the sequence, like the stale contract — the
post-iteration `Err()` check (below) reports what ended it.
(Pinned by `TestIteratorConstructionPanicsOnGuardErrors`,
`TestIteratorConstructionPanicsOnClosedDB`, and
`TestTypedIteratorConstructionPanicsEagerly`.)

```go
// GuardIterConstruction fires the same construction-time panic the
// keyspace's own iterators fire when the transaction is unusable
// or the keyspace handle is dead. For tiers that wrap iteration
// surfaces (gmdb/typed) and must fail their error short-circuit
// paths identically; callers rarely need it. SetKeyspace carries
// the same method.
func (ks *Keyspace) GuardIterConstruction()
```

Mutation during iteration: a loop-body mutation on the same keyspace
stales the iterator's cursor and ENDS the sequence early (the Seq2
error model has no channel; the stale surfaces as end-of-sequence,
with `Err() == ErrCursorStale` post-loop) — recovery is a fresh
All/Range/Prefix. Iterator cursors are registered
with the keyspace only while the loop is live and unregister at loop
exit (completed or broken), unlike explicit Cursor() handles, which
stay registered — and re-positionable — for the transaction lifetime.

## Index Lookup API

```go
type IndexHandle struct { /* unexported */ }

// IterOption configures an index-handle iteration surface
// (Lookup / LookupKeys / Range / Prefix; Get takes none — it
// yields at most one row). The one option is Reverse().
type IterOption func(*iterConfig)

// Reverse makes the iteration yield the same entry SET in exactly
// reversed order: the element-wise reversal of the forward
// sequence over the same snapshot, same-tx dirty state included.
// No-op on a unique index's Lookup / LookupKeys. The
// handle-invalidation contract is direction-blind (Inv-IHS1..5
// apply as written).
func Reverse() IterOption

// Lookup returns (pk, value) pairs matching the **exact** column
// tuple. The number of supplied cols MUST equal the index's
// declared column count; supplying fewer or more sets idx.Err()
// to a wrapped ErrInvalidOptions and yields nothing — use Prefix
// for partial-cols semantics (user-locked exact-cols enforcement).
//
// value is the on-disk encoded covering tuple when the index
// declares Covering — decode via DecodeCoveringTuple to recover
// the extractor's per-column Cover bytes. When the index has no
// Covering declaration, value is fetched via back-lookup against
// the row keyspace and is the row's stored bytes verbatim.
// Iteration ends when no more matches; check Err() for errors.
//
// Intra-transaction consistency: index cursor and back-lookup both
// read the current transaction's dirty state. Row writes and index
// updates happen atomically in the same Put/Delete/Cursor.Delete,
// so a back-lookup for an index entry always finds the row. If a
// back-lookup ever fails to find its PK (engine bug or external
// corruption), the entry is silently skipped from iteration and the
// inconsistency is reportable via Check().
//
// Handle invalidation (indexing.md §Handle Invalidation): a
// mid-iter mutation that CoWs or frees this index's data tree
// pages (TxIndexes.Rebuild for this name; TxIndexes.Drop for this
// name; Put / Delete / Cursor.Delete on the parent indexed
// keyspace; the SetKeyspace mutator analogues) MarkStale's the
// iter's cursor — the next yield surfaces nothing and idx.Err()
// reports ErrCursorStale. The caller's recovery is to re-iterate
// on a fresh idx.Lookup, which opens a new cursor on the current
// post-mutation pinned.root. After TxIndexes.Drop on this name the
// handle becomes dead — subsequent Lookup/LookupKeys/Range/
// Prefix/Get/Stats return ErrIndexNotFound (the same sentinel
// ks.Index(name) returns for a now-missing index). After
// Tx.DeleteKeyspace on the parent keyspace the same calls return
// ErrKeyspaceClosed — the whole keyspace is gone, not just this
// index; the parent-dead sentinel wins over the per-handle dead
// sentinel (mirroring Cursor.Err's dead-check ordering).
func (idx *IndexHandle) Lookup(cols [][]byte, opts ...IterOption) iter.Seq2[[]byte, []byte]

// LookupKeys returns matching primary keys without back-lookup or
// covering decode. Iteration cost is O(matches) leaf scans only.
// The same exact-cols validation as Lookup applies (user-locked
// user-locked). Same Inv-IHS3 / Inv-IHS2 dead-handle contract as
// Lookup: post-DeleteKeyspace → idx.Err() = ErrKeyspaceClosed;
// post-DropIndex → idx.Err() = ErrIndexNotFound wrap.
//
// Because LookupKeys never probes the row keyspace, it does not
// observe missing-PK inconsistencies (the silent-skip case noted
// on Lookup) — every index entry yields its raw PK, even if the
// corresponding row has somehow vanished. Use Check() for
// row/index consistency verification.
func (idx *IndexHandle) LookupKeys(cols [][]byte, opts ...IterOption) iter.Seq[[]byte]

// Range returns matches in [start, end). Each tuple is a slice of
// per-column byte slices; nil tuple = open-ended. Same value
// semantics as Lookup: covering tuple (decode via
// DecodeCoveringTuple) when IndexDecl.Covering is non-empty, row
// bytes via back-lookup otherwise. Same Inv-IHS3 / Inv-IHS2
// dead-handle contract as Lookup: post-DeleteKeyspace →
// idx.Err() = ErrKeyspaceClosed; post-DropIndex → idx.Err() =
// ErrIndexNotFound wrap.
//
// Partial-tuple bounds are **prefix-bounds**: a start (or end) with
// fewer columns than the index's declared count acts as the lex-
// encoded prefix of that leading-cols group. Concretely, for a
// 2-column index (color, size):
//
//   - Range([[0x42]], [[0x43])) matches every row whose first col
//     == 0x42 (full tuple group bounded below by 0x42 inclusive,
//     above by 0x43 exclusive).
//   - Range([[0x42, 0x10]], [[0x42, 0x20])) matches rows with
//     (0x42, 0x10) inclusive ... (0x42, 0x20) exclusive.
//   - Range(nil, [[0x42])) matches every row whose first col < 0x42.
//
// The bound is the lex-encoded byte sequence, not a semantic
// tuple comparison — Range works on the on-disk encoded form, so
// shorter tuples naturally sort before longer ones sharing the
// same lead bytes. Use Prefix for the equivalent "leading cols ==
// X" query (single-bound shorthand).
//
// A bound with MORE columns than the index declares can never
// match the encoding: Err() reports ErrInvalidOptions (matching
// Lookup / LookupKeys / Prefix). Fewer columns is the documented
// prefix-bound semantics above.
func (idx *IndexHandle) Range(start, end [][]byte, opts ...IterOption) iter.Seq2[[]byte, []byte]

// Prefix returns matches whose leading columns equal the prefix.
// The number of leading columns must be ≤ the index's declared
// column count — supplying more returns idx.Err() set to a wrapped
// ErrInvalidOptions and yields nothing.
//
// Equivalent to Range(prefix, nextPrefix) where nextPrefix is the
// smallest tuple strictly greater than the prefix; callers using
// Prefix don't need to compute that upper bound themselves. Same
// value semantics and Inv-IHS3 / Inv-IHS2 dead-handle contract as
// Lookup.
func (idx *IndexHandle) Prefix(leadingCols [][]byte, opts ...IterOption) iter.Seq2[[]byte, []byte]

// IndexEntryKey is one index entry's decoded key: the per-column
// byte tuple in declaration order plus the row's primary key.
type IndexEntryKey struct {
    Cols [][]byte
    PK   []byte
}

// RangeEntries yields the index's stored entries in [start, end)
// as (entry key, stored value bytes): the decoded column tuple +
// PK, and the entry's value VERBATIM — the encoded covering tuple
// (decode via DecodeCoveringTuple), empty when the declaration
// carries no covering. Unlike Range it performs NO back-lookup
// and NO covering-route interpretation: it is the raw entry
// surface for callers composing their own value acquisition (the
// query executor's index-only plans — query-builder.md
// §Covering-aware execution). Because it never probes the row
// keyspace it does not observe the silent-skip case (exactly like
// LookupKeys). Same partial-tuple prefix-bounds, IterOption
// surface, Err contract, and handle-invalidation behavior as
// Range. Keyspace indexes only: a SetKeyspace index's compound-PK
// key encoding does not decode as a plain column tuple, and its
// natural result — the (setKey, setValue) pair — is already
// served by the existing surfaces; calling RangeEntries on one
// sets Err to an ErrInvalidOptions wrap and yields nothing.
func (idx *IndexHandle) RangeEntries(start, end [][]byte, opts ...IterOption) iter.Seq2[IndexEntryKey, []byte]

// Decl returns the handle's pinned index declaration — the live
// decl this handle serves under (indexing.md §Handle Invalidation).
// For tiers that must inspect the declaration shape (gmdb/typed's
// full-row covering recognition); treat as read-only.
func (idx *IndexHandle) Decl() *IndexDecl

// EnableCoverValueReturn opts this handle into the byte-layer
// full-row covering-return route: Lookup / Range / Prefix / Get
// return the entry's stored encode(V) instead of back-looking-up
// the row — valid ONLY for a decl the engine recognizes as
// full-row covering (the gmdb/cover-value/ sentinel; the route
// re-checks the LIVE decl on every read, so a same-tx rebuild
// that removes the sentinel downgrades the handle safely).
// CoverValueReturnEnabled is the read-side companion. Used by
// gmdb/typed; callers rarely need either.
func (idx *IndexHandle) EnableCoverValueReturn()
func (idx *IndexHandle) CoverValueReturnEnabled() bool

// Get is shorthand for unique indexes: returns the single (pk, value)
// or ErrNotFound. Returns ErrIndexNotUnique when called on a
// non-unique index. Returns ErrKeyspaceClosed (Inv-IHS3) when the
// parent keyspace was DeleteKeyspace'd in this tx, and
// ErrIndexNotFound (Inv-IHS2 wrap) when this index was Drop'd —
// both checked at entry before any descent. Same value semantics
// as Lookup: covering tuple (decode via DecodeCoveringTuple) when
// IndexDecl.Covering is non-empty, row bytes via back-lookup
// otherwise.
func (idx *IndexHandle) Get(cols ...[]byte) (pk, value []byte, err error)

// Err returns the broader handle-invalid sentinel if the parent
// keyspace was DeleteKeyspace'd (Inv-IHS3 → ErrKeyspaceClosed,
// wins over the sticky iter cause because re-position-to-recover
// is impossible when the parent is gone), otherwise the first
// error encountered during the last sequence returned by Lookup /
// Range / Prefix / LookupKeys (Inv-IHS1 sticky), otherwise the
// post-Drop dead-handle sentinel (Inv-IHS2 → wrapped
// ErrIndexNotFound), otherwise nil.
//
// Index handles are not safe for concurrent use by multiple
// goroutines. The Err state is per-handle, so two overlapping
// iterators on the same *IndexHandle would race. Open the keyspace in
// separate transactions, or call ks.Index(name) once per goroutine,
// for concurrent index queries.
func (idx *IndexHandle) Err() error

// Stats returns the index's persistent count + tree statistics, or
// ErrKeyspaceClosed (Inv-IHS3) when the parent keyspace was
// DeleteKeyspace'd in this tx, or ErrIndexNotFound (Inv-IHS2
// wrap) when this index was Drop'd. Stats does NOT clear the
// sticky iter cause on idx.err (Inv-IHS1) — the cause remains
// observable via idx.Err() until a fresh iter resets it.
func (idx *IndexHandle) Stats() (IndexStats, error)

// DecodeCoveringTuple decodes the byte slice returned by Lookup /
// Get / Range / Prefix on an index whose IndexDecl.Covering is
// non-empty (the byte-API covering return contract — see
// indexing.md §Covering Indexes). The returned [][]byte has one
// entry per declared IndexCoveringColumn in declaration order, each
// carrying the extractor's IndexEntry.Cover[i] bytes verbatim.
//
// Returns an error wrapping ErrCoveringTupleMalformed (declared
// in §Sentinel errors above) if the input does not parse as a
// NUL-escape column tuple. The wrap is neutral by design: an
// on-disk-corruption diagnosis goes through Check(), not this
// decoder's error class.
func DecodeCoveringTuple(value []byte) ([][]byte, error)
```

The typed tier bridges to this surface via
`typed.KeyspaceHandle.ByteIndex(name)` — it returns a FRESH
`*IndexHandle` per call (exactly like `Keyspace.Index`), which is
what lets the query executor obtain one handle per plan leaf per
execution so concurrently-draining iterators never share Err
state (`typed-keyspaces.md §Typed Indexes`, `query-builder.md
§Byte-surface requirements`).

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

    // SpilledPages counts slab pages written out to their file
    // locations before commit because the working set exceeded the
    // MaxTxBufferBytes spill threshold (pager-slab.md §Slab
    // Budget). Non-zero means the engine traded early pwrites for
    // bounded memory; a persistently large value suggests raising
    // MaxTxBufferBytes.
    SpilledPages uint64

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
    // IndexEntriesInserted counts index-tree Puts, INCLUDING
    // in-place covering rewrites (same key, changed payload) — so
    // Inserted - Deleted is not the net entry-count delta when
    // covering updates occurred.
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
    // "BadPageChecksum", "RPLSegmentChecksum"). Stable across gmdb
    // versions for the purposes of tooling that pattern-matches on
    // issues; new codes may be added but existing ones never change
    // meaning. Every token is single-sourced as a code constant and
    // value-pinned (INV enforcement:
    // TestCheckIssueCodeTokensPinned). Use Code for programmatic
    // decisions; use Message for human-facing display.
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
// checksums (including RPL segment footers on the chain walk),
// B+tree integrity — page structure AND tree-level ordering: leaf
// and branch keys strictly increasing, separator routing
// (max(left) < S <= min(right)) threaded through a dedicated
// ordering pass — bitmap consistency, RPL chain, page accounting,
// prefix-compression integrity, keyspace descriptor consistency
// (descriptor Count vs the tree's actual entry/value count;
// meta.NumKeyspaces vs the descriptor tree), nested-tree member
// counts (NestedCount vs the actual subtree), and set keyspace
// subpage integrity. The ordering/count pass is one extra read
// traversal per tree over the page-cache-resident snapshot.
// Returns issues as an iter.Seq.
//
// Walk failures (I/O errors, unreadable pages) are reported as
// CheckFatal severity and are always the last issue yielded.
//
// Check internally opens a read transaction, lazily, on the first
// iteration (abandoning the Seq without ranging it opens nothing). The
// reader slot is released DETERMINISTICALLY when the iterator is
// exhausted OR when the caller breaks out of the range loop — the
// range-over-func protocol runs the closure's deferred Rollback in both
// cases, so an early break releases the slot promptly. A
// runtime.AddCleanup on the read transaction is only the GC backstop
// for a caller that abandons the Seq mid-iteration without completing
// or breaking; only that path waits on GC.
func (db *DB) Check() iter.Seq[CheckIssue]

type CheckOptions struct {
    // Repair enables offline repair: reclaims leaked pages (allocated in
    // the bitmap, unreachable from every committed tree, absent from the
    // RPL — the BitmapLeak class) by freeing them in the bitmap.
    //
    // Repair requires EXCLUSIVE access (no concurrent readers or
    // writers). CheckWithOptions opens a WRITE transaction for the Repair
    // mode (acquiring the cross-process write lock, so no other writer
    // runs concurrently) and proceeds only when no read transaction is
    // active in any process. With a reader active it frees nothing and
    // emits a single CheckError "Repair.ReadersActive"; run Check without
    // Repair for read-only diagnostics in that case.
    //
    // Repair is conservative: it frees a page ONLY when the structural
    // walk both completed (the caller did not break) and emitted NO
    // CheckError/CheckFatal, AND the RPL chain walk reached its
    // authoritative tail or a reclaimed boundary. Any structural finding
    // makes the reachable set unreliable — a page under a walk-aborting
    // corrupt subtree would be misclassified as leaked — and an RPL walk
    // truncated at a corrupt-segment (footer/decode) boundary hides
    // still-pending segments whose entries then misclassify as leaked
    // (background-maintenance.md §Bitmap Leak Reclamation) — so in either
    // case the database reports its would-be leaks with Repaired=false
    // plus a CheckWarning "Repair.Skipped" and reclaims nothing.
    // Reclaimed pages are reported as the usual BitmapLeak
    // CheckWarning with Repaired=true.
    //
    // The freed bitmap is published through the normal commit pipeline
    // (atomic meta-swap), so a crash mid-repair leaves either the
    // pre-repair bitmap or the fully-repaired one. The Repair mode may
    // also emit, as CheckFatal, "Repair.WriteTxUnavailable" (the
    // exclusive write tx could not be opened), "Repair.FreeFailed", or
    // "Repair.CommitFailed" (nothing was reclaimed).
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
    //
    // The supplied IndexDecl's Unique and Covering must match the
    // registered index: the check reproduces the on-disk (key, value)
    // from the SUPPLIED decl, so a mismatched Unique/Covering yields a
    // FingerprintDrift (correctly — the supplied decl does not describe
    // the stored index). Both Keyspace and SetKeyspace indexes are
    // verified, each with its own on-disk codec (a SetKeyspace's
    // extractor is re-run over every (set key, member) pair).
    //
    // The pass may also emit these stable "CheckIndexes."-prefixed
    // diagnostic codes: ExtractorMissing / ExtractorError (CheckError —
    // the supplied Extract is nil, or failed/violated uniqueness when
    // re-run); RowsUnreadable / IndexUnreadable (CheckWarning — a corrupt
    // tree blocked enumeration, already reported structurally);
    // KeyspaceKindUnsupported (CheckWarning — a keyspace kind the pass
    // cannot verify).
    Indexes map[string][]*IndexDecl
}

func (db *DB) CheckWithOptions(opts *CheckOptions) iter.Seq[CheckIssue]

// CopyTo creates a consistent copy at the given path. Taken from a
// read-tx snapshot — writers are not blocked (the snapshot's reader slot
// pins its pages against RPL reclamation for the whole copy, so a
// concurrent commit cannot reuse a page the copy reads). When compact is
// true, the copy is compacted: every B+tree is rebuilt bottom-up from its
// existing entries with page ids assigned sequentially from the first data
// page with NO gaps and free pages omitted, and the file shrinks to the
// live size. Page ids are globally gap-free, NOT per-tree contiguous — the
// data, index, registry, and descriptor trees and overflow runs draw from
// one monotonic counter, so a single tree's pages form a monotone-
// increasing but not necessarily adjacent id set. Index trees are rebuilt
// structurally from their stored entries (the extractor closures are not on
// disk). Inherits source's PageSize, BitmapPages, MaxSize.
//
// path must NOT already exist (CopyTo never clobbers an existing file).
// The copy receives a FRESH UUID — it is a distinct database identity,
// not a clone of the source's. Its allocation bitmap is REBUILT from the
// snapshot's reachable page set rather than copied from the source's
// (in-place-mutated) bitmap region, so the copy's free list is always
// consistent with its tree (leaked pages in the source are dropped, and a
// writer committing mid-copy cannot make the copy's bitmap disagree with
// its snapshot tree). The copy's RPL is empty: pages the source held
// pending reader-pinned reclamation are unreferenced in the copy and
// become free space.
//
// Destination crash-consistency: the copy is written to a
// `<path>.copytmp-*` temp file in path's directory, fsynced, then
// published at path — a crash mid-copy never leaves a partial file at
// path, only the inert temp (safe to delete once no CopyTo is in
// flight). The publish walks a per-filesystem atomicity ladder: an
// atomic hard link (the authoritative no-clobber guard — a file
// appearing at path mid-copy fails the publish EEXIST with the
// pre-existing file untouched); destinations without hard-link
// support (vfat/exfat, many FUSE mounts) fall back to a no-replace
// rename — renameat2(RENAME_NOREPLACE) on Linux, still atomic
// no-clobber; probe-then-rename where NOREPLACE is unavailable and
// off Linux, where no-clobber is best-effort (a real TOCTOU window).
// The renamed inode is always complete and fsynced; the rename
// rung's crash-atomicity is the destination filesystem's own (FAT-
// class metadata can tear the dirent on power loss mid-rename). A
// failed link with path already naming the copied inode — NFS's
// link retransmission — counts as a successful publish.
//
// Windows share-mode and replace contract: the data and lock files
// are opened via os.Root, whose windows opens request
// FILE_SHARE_READ|WRITE|DELETE; the plain (share-delete-less) opens
// are all off the publish/replace path — copy temps (CopyTo's and
// Compact's, which no other handle opens before publish), bulk-load
// spill scratch files, and transient directory handles. No gmdb
// code path may
// introduce a restrictive share mode on the publish/replace path.
// CopyTo's publish (hard link or no-clobber rename of a FRESH name)
// replaces nothing that is open and works unchanged; its no-clobber
// rung is the same probe-then-rename best-effort as every non-Linux
// platform.
//
// Compact's in-place replace does NOT work on windows: the kernel
// refuses to replace or delete a file any process holds MAPPED
// (ERROR_USER_MAPPED_FILE class), and every gmdb handle maps the
// data file — including the compacting handle itself, whose old
// mapping is still live at rename time. Windows Compact therefore
// fails deterministically with a clean error at its publish rename:
// the temp is removed, the database is untouched, the handle
// remains usable. Lifting the limitation requires a dedicated
// windows design meeting all of: the caller tears down its own
// mapping before the rename, no peer process maps the file (a
// sole-mapper precondition), and the rename uses os.Root's
// POSIX-semantics form — recorded here so any future design starts
// from the real precondition set.
//
// A verbatim (compact=false) copy of a source whose file no longer
// covers its meta's HighWaterMark — a truncated transfer, a forged
// meta — clamps its walk to the file-resident extent, exactly as
// Check does, and fails with a corruption-class error when the tree
// references the missing tail (checksums.md §Structural and
// Allocation Bounds: error, never a SIGBUS). When the tree is intact
// below the clamp, the copy SUCCEEDS and its meta carries the CLAMPED
// HighWaterMark — the copy describes the file it actually is, never
// the source's forged claim (pinned by
// TestCopyToForgedHWMClampsCopyMeta).
//
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
//  4. fsync(tmpPath); atomic rename(tmpPath, originalPath); fsync the
//     containing directory (so the rename itself is durable across a
//     crash); reopen this handle's data-file fd + writer mmap against
//     the new inode (the lock file is not renamed — the Coord + write
//     grant persist across the reopen); release the cross-process write
//     lock. The temp file is created with the same mode as the source
//     DB (0600) so the rename does not widen permissions.
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
// Peer HANDLES post-rename (cross-process.md §Data-file generation):
// a peer process's already-open DB handle keeps its fd/mmap on the
// replaced inode. Compact bumps the lock header's DataGeneration
// (under the write grant, after the rename + directory fsync, before
// its own reopen), and every handle re-checks the field after each
// write-grant acquisition and reader-slot publish: the stale peer's
// next Begin / BeginRead / Checkpoint / Compact poisons its handle
// and returns ErrPoisoned instead of committing to (or reading) the
// unlinked file — the silent cross-process fork the all-or-nothing
// invariant forbids. Close + re-Open converges the peer on the new
// inode.
//
// Reopen failure: if the reopen in step 4 fails AFTER the rename
// succeeded (the on-disk file is the new inode, but this handle could
// not remap it), the handle is POISONED — every subsequent operation
// returns ErrPoisoned — rather than left silently serving the stale,
// now-unlinked inode (the split-brain the all-or-nothing invariant
// forbids). Close + re-Open recovers against the renamed file.
//
// Compact MUST NOT be called from a goroutine holding an open write
// transaction on this DB: it acquires the write lock and would block
// forever behind the grant the caller already holds.
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

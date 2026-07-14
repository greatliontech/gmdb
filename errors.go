package gmdb

import "errors"

// Sentinel errors. This surface is intentionally small; the full
// inventory from api-surface.md is added as each surfacing path arrives.
var (
	// ErrInvalidOptions is returned by Open for any Options-validation
	// failure. Wrap a specific sub-error (errInvalidPageSize, etc.).
	ErrInvalidOptions = errors.New("gmdb: invalid options")

	// ErrTxClosed is returned by Tx methods after Commit / Rollback.
	ErrTxClosed = errors.New("gmdb: transaction is closed")

	// ErrReadOnly is returned by mutating methods on a read transaction
	// (BeginRead / View) or a read-only keyspace handle (OpenKeyspace-
	// ReadOnly). Distinct from ErrDatabaseReadOnly, which signals the
	// whole DB was opened read-only.
	ErrReadOnly = errors.New("gmdb: read-only transaction")

	// ErrDatabaseReadOnly is returned by the write entry points (Begin /
	// Update / Batch / Compact / Checkpoint) when the database was
	// opened with Options.ReadOnly. The writer pager path is never
	// initialised, so writes are rejected at the door rather than
	// failing partway. Reads (BeginRead / View) work normally.
	ErrDatabaseReadOnly = errors.New("gmdb: database opened read-only")

	// ErrBatchClosurePanic wraps a panic raised inside a DB.Batch
	// closure. The coordinator recovers the panic, rolls back that
	// closure's child transaction, and returns this (wrapping the panic
	// value) to that one caller; sibling closures and the parent batch
	// are unaffected. Per transactions.md §Write Batching.
	ErrBatchClosurePanic = errors.New("gmdb: batch closure panicked")

	// ErrBatchClosureGoexit is returned by Batch when the closure exited
	// via runtime.Goexit (t.FailNow and friends) instead of returning:
	// the coordinator contains the unwind exactly like a panic — the
	// child is rolled back, siblings and the parent batch are unaffected
	// (transactions.md §Write Batching).
	ErrBatchClosureGoexit = errors.New("gmdb: batch closure exited via runtime.Goexit")

	// ErrChildActive is returned by the frozen operations on a write
	// transaction that has an unresolved child transaction open
	// (created via Tx.BeginChild) — data ops, Commit, and a second
	// BeginChild. The parent, and transitively every ancestor, is
	// frozen until the active child commits or rolls back. Rollback is
	// the exception: it cascade-rolls-back the open descendant chain
	// deepest-first and then the transaction itself. Per
	// transactions.md §Nested Transactions (LMDB-style parent-freeze).
	ErrChildActive = errors.New("gmdb: transaction is frozen by an active child transaction")

	// ErrTxTooLarge is returned when the per-tx slab budget is
	// exceeded.
	ErrTxTooLarge = errors.New("gmdb: transaction buffer budget exceeded")

	// ErrDBFull is returned when no free space remains and the file
	// cannot grow further.
	ErrDBFull = errors.New("gmdb: database is full")

	// ErrCorrupted is returned when a structural inconsistency is
	// detected (e.g. a malformed RPL segment, an unrecoverable meta
	// page, a tree whose Type/Count fields don't match the cell
	// layout). Wraps a more specific sub-error where possible.
	ErrCorrupted = errors.New("gmdb: database is corrupted")

	// ErrBadPageChecksum is returned when the XXH3-64 footer on a
	// data page does not match the page content. Wraps with the
	// page ID where useful.
	ErrBadPageChecksum = errors.New("gmdb: page checksum mismatch")

	// ErrVersionMismatch is returned by Open when the database file is
	// an intact gmdb file written by a different, incompatible on-disk
	// format version (the meta page's Version != the engine's
	// FormatVersion). Distinct from ErrCorrupted: the file is not
	// damaged — it is a format this binary cannot read, so the operator
	// should use a matching gmdb version. See file-layout.md §Meta Page
	// (format-version bump) for the evolution mechanism.
	ErrVersionMismatch = errors.New("gmdb: on-disk format version mismatch")

	// ErrPoisoned is returned by every transaction-opening operation
	// (Begin / BeginRead / Update / Compact / Checkpoint) after the
	// handle is poisoned. Three causes: (a) a previous write
	// transaction's commit failed in the publication phase (step-3
	// pwrite or step-4 fdatasync), leaving the on-disk active meta
	// potentially advanced while the in-memory pager state has been
	// rolled back to pre-tx; (b) a Compact reopen failed after the
	// rename, leaving the handle mapping the stale, now-unlinked
	// inode; (c) a Checkpoint failed in its publication phase
	// (durability.md §Checkpoint failure semantics — a retried
	// checkpoint after a consumed fsync error would falsely certify
	// non-durable data).
	// In case (b) even reads are unsafe (they would observe pre-Compact
	// data), so BeginRead is rejected too. The caller must Close() and
	// re-Open() to recover. Close() works normally on a poisoned handle.
	ErrPoisoned = errors.New("gmdb: database handle is poisoned; Close and re-Open to recover")

	// ErrCompactReadersActive is returned by Compact when active
	// in-process read transactions do not drain within
	// Options.CompactDrainTimeout. Compact needs the in-process readers
	// gone before swapping the file; if they persist, use
	// CopyTo(path, compact=true) to produce an offline compacted copy
	// instead (api-surface.md §Compact). No file swap occurred.
	ErrCompactReadersActive = errors.New("gmdb: Compact drain timed out — in-process read transactions still active")

	// ErrClosed is returned by operations against a DB handle whose
	// Close has been called (or is concurrently in progress). Surfaces
	// from Begin when the cross-process coordinator's goroutine has
	// already shut down. The full close-gate semantics are specified
	// by cross-process.md §Heartbeat Goroutine + leak-detection.md
	// §Close Ordering — this sentinel is the user-facing surface
	// that contract requires.
	ErrClosed = errors.New("gmdb: database is closed")

	// ErrReadersFull is returned by BeginRead / View when every
	// reader slot in the lock-file table is occupied (cross-process.md
	// §Reader Table, transactions.md §Read Transaction step 2). With
	// a deadline-bearing context the call retries until a slot
	// becomes free or the deadline fires; with no deadline the call
	// returns ErrReadersFull immediately so callers can distinguish
	// "table at capacity" from other Begin failures. Also returned
	// (wrapped) when the begin's freshly-acquired slot was aged out
	// mid-acquisition — reachable only when the process stalled past
	// StaleTimeout inside BeginRead; retrying acquires a fresh slot.
	ErrReadersFull = errors.New("gmdb: no reader slots available")

	// ErrNotFound is returned by keyed-removal and lookup APIs when
	// the addressed item is absent: Tx.OpenKeyspace on a missing
	// keyspace name; Tx.DeleteKeyspace on a missing name;
	// Keyspace.Delete / SetKeyspace.Delete / SetKeyspace.DeleteValue
	// per the Delete-on-miss invariant
	// (api-surface.md §Invariants).
	ErrNotFound = errors.New("gmdb: key not found")

	// ErrKeyExists is returned by Tx.CreateKeyspace and
	// Tx.CreateSetKeyspace when a keyspace with the supplied name
	// already exists. CreateKeyspaceIfNotExists silently opens the
	// existing keyspace instead.
	ErrKeyExists = errors.New("gmdb: key already exists")

	// ErrKeyEmpty is returned by every operation taking a key when
	// the key argument is nil or zero-length, per the api-surface.md
	// §Invariants clause-explicit invariant. Empty keys are invalid
	// at the API surface because they ambiguate the "no key found"
	// sentinel.
	ErrKeyEmpty = errors.New("gmdb: key is nil or empty")

	// ErrKeyTooLarge is returned by Put / Delete / Get and BulkLoad when
	// a key exceeds the maximum size — too large even for an
	// overflow-reference leaf entry (limits.md §Maximum Key Size; the
	// per-key cap is ~(PageSize-40)/2). Values never trip it: oversize
	// values promote to an overflow chain. The internal
	// btree.ErrKeyTooLarge is translated to this public sentinel by
	// mapBtreeErr so callers' errors.Is(err, ErrKeyTooLarge) works.
	ErrKeyTooLarge = errors.New("gmdb: key exceeds maximum size")

	// ErrKeyspaceKindMismatch is returned by Tx.OpenKeyspace when
	// the stored descriptor's Kind does not match the API used (e.g.
	// OpenKeyspace on a Kind=1 SetKeyspace, OpenSetKeyspace on a
	// Kind=0 Keyspace) per keyspaces.md invariant #3. The call leaves
	// state unmodified.
	ErrKeyspaceKindMismatch = errors.New("gmdb: keyspace kind does not match existing keyspace")

	// ErrKeyspaceReserved is returned by Tx.OpenKeyspace /
	// OpenSetKeyspace / DeleteKeyspace when the supplied name
	// resolves to an engine-internal keyspace (Kind=2 — per-index
	// storage). These are addressable only via their parent
	// keyspace's index registry, never by name through the user API.
	ErrKeyspaceReserved = errors.New("gmdb: keyspace name reserved for engine use")

	// ErrCursorUnpositioned is returned by Cursor methods that
	// require Positioned state (Current / Delete) when the cursor
	// is Unpositioned or at End-of-iteration. The caller re-
	// positions via First / Last / Seek / SeekGE.
	ErrCursorUnpositioned = errors.New("gmdb: cursor not positioned")

	// ErrCursorStale is returned by Cursor non-repositioning ops
	// (Current / Next / Prev / Delete) when a sibling mutator
	// invalidated the cursor. Per transactions.md §Cursor State
	// Machine, the caller re-positions to recover.
	ErrCursorStale = errors.New("gmdb: cursor invalidated by sibling mutation")

	// ErrCursorClosed is returned by every Cursor / SetCursor
	// operation after the cursor was explicitly released via
	// Close(). Terminal for the cursor's lifetime — unlike
	// ErrCursorStale there is no re-position recovery; the caller
	// opens a fresh cursor. Per transactions.md §Cursor State
	// Machine (explicit cursor release).
	ErrCursorClosed = errors.New("gmdb: cursor closed")

	// ErrKeyspaceClosed is returned by Keyspace / SetKeyspace /
	// Cursor / SetCursor operations against a handle whose parent
	// keyspace was DeleteKeyspace'd within the same write
	// transaction (api-surface.md §Keyspace API DeleteKeyspace).
	// Invalidation is permanent for the handle's lifetime — even a
	// CreateKeyspace re-creating the same name does NOT reactivate
	// the old handle; the new CreateKeyspace returns a fresh
	// *Keyspace while the old handle stays dead until the caller
	// drops it.
	ErrKeyspaceClosed = errors.New("gmdb: keyspace closed by DeleteKeyspace")

	// ErrValueSizeMismatch is returned by SetKeyspace.Put /
	// SetKeyspace.DeleteValue when the value's length differs from
	// the keyspace's declared FixedValueSize. Per api-surface.md
	// §Sentinel errors + keyspaces.md invariant #5 (FixedValueSize
	// immutable + uniform for Kind=1).
	ErrValueSizeMismatch = errors.New("gmdb: value size does not match fixed value size")

	// ErrFixedValueSizeMismatch is returned by
	// Tx.CreateSetKeyspaceIfNotExists when the existing keyspace's
	// FixedValueSize differs from the supplied
	// opts.FixedValueSize. FixedValueSize is immutable after
	// creation (keyspaces.md invariant #5), so the API cannot
	// silently coerce the caller's opts to the existing value
	// without misleading the caller about the storage layout. Per
	// api-surface.md.
	ErrFixedValueSizeMismatch = errors.New("gmdb: keyspace exists with different FixedValueSize")

	// ErrIndexExtractorRequired is returned by Tx.OpenKeyspace /
	// Tx.OpenSetKeyspace when an IndexDecl is missing for an index
	// already declared in the keyspace's registry (the caller passed
	// fewer IndexDecls than the stored set), or by TxIndexes.Rebuild
	// when the supplied decl.Extract is nil. Per
	// indexing.md §Open Semantics + §Rebuild.
	ErrIndexExtractorRequired = errors.New("gmdb: index extractor required for OpenKeyspace")

	// ErrIndexUnknown is returned by Tx.OpenKeyspace /
	// Tx.OpenSetKeyspace when an IndexDecl is supplied for a name
	// that is NOT registered on the keyspace (the caller passed an
	// extra IndexDecl beyond the stored set). Per indexing.md §Open
	// Semantics.
	ErrIndexUnknown = errors.New("gmdb: IndexDecl supplied for index not declared in registry")

	// ErrIndexFingerprintMismatch is returned wrapped in
	// *IndexFingerprintError when an opened keyspace's stored
	// schema-hash or Version tag differs from the supplied
	// IndexDecl's. Caller's recovery path is TxIndexes.Rebuild (per
	// indexing.md §Drift Guard + §Rebuild).
	ErrIndexFingerprintMismatch = errors.New("gmdb: index fingerprint mismatch — RebuildIndex required")

	// ErrIndexUniqueViolation is returned by Put / Cursor.Delete on
	// an indexed keyspace when a unique-index probe detects a
	// duplicate index key — either against the existing on-disk
	// index, or within the candidate-set produced by a single
	// extractor invocation. Neither the row nor any index entries
	// are written. Per indexing.md §Unique Indexes.
	ErrIndexUniqueViolation = errors.New("gmdb: unique index violation")

	// ErrIndexEncoderIDReserved rejects an encoder ID inside the
	// reserved column namespace (gmdb/col/, gmdb/multicol/,
	// gmdb/cover-value/ — typed-keyspaces.md §Encoder interface):
	// the synthesized-name domains of the typed declaration forms
	// stay provably disjoint only because callers cannot mint IDs
	// inside them. Surfaced at OpenKeyspace / CreateKeyspace through
	// the typed tier's declaration lowering, before any work.
	ErrIndexEncoderIDReserved = errors.New("gmdb: encoder ID is inside the reserved column namespace")

	// ErrIndexKindUnknown rejects an IndexDecl whose Kind is not a
	// kind this engine version implements (indexing.md §Overview —
	// IndexKindComposite is currently the only kind). Surfaced at
	// OpenKeyspace / CreateKeyspace / RebuildIndex, before any work.
	ErrIndexKindUnknown = errors.New("gmdb: index kind unknown to this engine version")

	// ErrIndexNotUnique is returned by Index.Get when called on a
	// non-unique index. Get's contract (single (pk, value) result)
	// is only well-defined for unique indexes. Per indexing.md
	// §Lookup API.
	ErrIndexNotUnique = errors.New("gmdb: Get called on non-unique index")

	// ErrIndexExists is returned by Tx.OpenKeyspace /
	// Tx.OpenSetKeyspace / Tx.CreateKeyspace when the supplied
	// IndexDecl slice contains two entries with the same Name —
	// duplicate names are rejected at validation time (the offending
	// name is wrapped via fmt.Errorf("…: %w", ErrIndexExists)). Per
	// indexing.md §Index Declaration.
	ErrIndexExists = errors.New("gmdb: index already exists")

	// ErrIndexNotFound is returned by Keyspace.Index /
	// SetKeyspace.Index when no index with the supplied name is
	// registered on the keyspace, and by TxIndexes.Drop /
	// TxIndexes.Rebuild when the index name does not match any
	// registry entry on the keyspace. Distinct from ErrNotFound to
	// let callers dispatch between keyspace-missing
	// (ErrNotFound — keyspace-management dimension) and
	// index-name-missing (ErrIndexNotFound — index-management
	// dimension).
	ErrIndexNotFound = errors.New("gmdb: index not found")

	// ErrIndexEncoderIDEmpty is returned by typed.Index declaration
	// when the supplied Encoder[T].ID() returns "" — encoder IDs
	// must be unique non-empty strings for schema-hash
	// determinism. Per typed-keyspaces.md.
	ErrIndexEncoderIDEmpty = errors.New("gmdb: typed index encoder returned empty ID() — encoder IDs must be unique non-empty strings")

	// ErrBulkLoadOutOfOrder is returned by Keyspace.BulkLoad /
	// SetKeyspace.BulkLoad when the input stream yields a key (or, for
	// a SetKeyspace, a (key, value) pair) that is not strictly greater
	// than its predecessor. BulkLoad's bottom-up construction relies on
	// strictly-ascending input; a violation aborts the load before any
	// page becomes reachable from a recoverable meta. Per bulkload.md
	// §Invariants.
	ErrBulkLoadOutOfOrder = errors.New("gmdb: BulkLoad input not in ascending key order")

	// ErrBulkLoadNonEmpty is returned by Keyspace.BulkLoad /
	// SetKeyspace.BulkLoad when the target keyspace is not empty
	// (Count != 0). Bulk-loading into a non-empty keyspace would mix
	// bottom-up-constructed leaves with pre-existing top-down ones;
	// the load returns this sentinel without writing anything. Clear
	// first with ks.DeleteRange(nil, nil) if necessary. Per
	// bulkload.md §Invariants.
	ErrBulkLoadNonEmpty = errors.New("gmdb: BulkLoad requires an empty keyspace")

	// ErrKeyspaceAlreadyOpen is returned by Tx.OpenKeyspace /
	// Tx.OpenSetKeyspace when a second open call for the same name
	// within one transaction supplies an IndexDecl set that differs
	// from the first call's set by any hashable input (names,
	// Unique flags, schema hashes, Versions, and — for typed
	// indexes — encoder IDs). Also returned when mixing
	// OpenKeyspace and OpenKeyspaceReadOnly for the same name
	// within one transaction. Per indexing.md §Re-opening a
	// keyspace in the same transaction.
	ErrKeyspaceAlreadyOpen = errors.New("gmdb: keyspace already opened in this transaction with a different index set")
)

var (
	errInvalidPageSize             = errors.New("gmdb: PageSize must be a power of two in [4096, 65536]")
	errInvalidSizeBounds           = errors.New("gmdb: MaxSize must be > 0 and >= MinSize")
	errInvalidTxBuffer             = errors.New("gmdb: MaxTxBufferBytes must be > 0")
	errInvalidMaxReaders           = errors.New("gmdb: MaxReaders must be in [1, 65536]")
	errInvalidSyncMode             = errors.New("gmdb: SyncMode out of range")
	errInvalidMergeThreshold       = errors.New("gmdb: MergeThreshold must be in [1, 50]")
	errInvalidRestartGroupTarget   = errors.New("gmdb: RestartGroupTarget must be in [0, 255]")
	errInvalidLeafLayout           = errors.New("gmdb: unknown LeafLayout")
	errInvalidBranchLayout         = errors.New("gmdb: unknown or unsupported BranchLayout")
	errInvalidMaxBatchSize         = errors.New("gmdb: MaxBatchSize must be >= 1")
	errInvalidMaxBatchDelay        = errors.New("gmdb: MaxBatchDelay must be >= 0")
	errInvalidMaintenance          = errors.New("gmdb: invalid MaintenanceOptions (Interval/ScrubBatchSize/CompactionBatchSize must be >= 0; CompactionThreshold in [0,1])")
	errInvalidCoordInterval        = errors.New("gmdb: StaleTimeout/HeartbeatInterval/LockRetryInterval must be >= 0")
	errStaleTimeoutTooSmall        = errors.New("gmdb: StaleTimeout must be > HeartbeatInterval (cross-process.md §Heartbeat Goroutine: significantly larger, for scheduling jitter)")
	errCrossNSStaleTimeoutTooSmall = errors.New("gmdb: CrossNamespaceStaleTimeout must be >= StaleTimeout (cross-process.md §Stale-reader detection: the cross-namespace window widens, never tightens)")
)

package gmdb

import "errors"

// Sentinel errors. The chunk-1 surface is intentionally small; the full
// inventory from api-surface.md lands as each surfacing path arrives.
var (
	// ErrInvalidOptions is returned by Open for any Options-validation
	// failure. Wrap a specific sub-error (errInvalidPageSize, etc.).
	ErrInvalidOptions = errors.New("gmdb: invalid options")

	// ErrTxClosed is returned by Tx methods after Commit / Rollback.
	ErrTxClosed = errors.New("gmdb: transaction is closed")

	// ErrReadOnly is returned by mutating methods on a read transaction.
	ErrReadOnly = errors.New("gmdb: read-only transaction")

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

	// ErrBadPageChecksum is returned when the xxhash64 footer on a
	// data page does not match the page content. Wraps with the
	// page ID where useful.
	ErrBadPageChecksum = errors.New("gmdb: page checksum mismatch")

	// ErrVersionMismatch is returned when the on-disk format version
	// does not match the engine's FormatVersion. Reserved for future
	// format evolutions; never returned in v0.
	ErrVersionMismatch = errors.New("gmdb: on-disk format version mismatch")

	// ErrPoisoned is returned by Begin / Update after a previous
	// transaction's commit failed in the publication phase (step-3
	// pwrite or step-4 fdatasync), leaving the on-disk active meta
	// potentially advanced while the in-memory pager state has been
	// rolled back to pre-tx. The DB handle is in a state where it
	// cannot safely allocate new pages (its in-memory bitmap /
	// HighWaterMark / RPL chain disagree with disk); the caller must
	// Close() and re-Open() to recover. Close() works normally on a
	// poisoned handle.
	ErrPoisoned = errors.New("gmdb: database handle is poisoned; Close and re-Open to recover")

	// ErrClosed is returned by operations against a DB handle whose
	// Close has been called (or is concurrently in progress). Surfaces
	// from Begin when the cross-process coordinator's goroutine has
	// already shut down. The full db.closed semantics (cross-process.md
	// §Heartbeat Goroutine + leak-detection.md §Close Ordering) are
	// promoted to a spec-tier project invariant in a later sub-chunk
	// — this sentinel is the user-facing surface that invariant
	// requires.
	ErrClosed = errors.New("gmdb: database is closed")

	// ErrReadersFull is returned by BeginRead / View when every
	// reader slot in the lock-file table is occupied (cross-process.md
	// §Reader Table, transactions.md §Read Transaction step 2). With
	// a deadline-bearing context the call retries until a slot
	// becomes free or the deadline fires; with no deadline the call
	// returns ErrReadersFull immediately so callers can distinguish
	// "table at capacity" from other Begin failures.
	ErrReadersFull = errors.New("gmdb: no reader slots available")

	// ErrNotFound is returned by keyed-removal and lookup APIs when
	// the addressed item is absent: Tx.OpenKeyspace on a missing
	// keyspace name; Tx.DeleteKeyspace on a missing name;
	// Keyspace.Delete / SetKeyspace.Delete / SetKeyspace.DeleteValue
	// per the chunk-5.1 Delete-on-miss invariant
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
	// chunk-6.1 spec amendment.
	ErrFixedValueSizeMismatch = errors.New("gmdb: keyspace exists with different FixedValueSize")

	// ErrIndexExtractorRequired is returned by Tx.OpenKeyspace /
	// Tx.OpenSetKeyspace when an IndexDecl is missing for an index
	// already declared in the keyspace's registry (the caller passed
	// fewer IndexDecls than the stored set), or by Tx.RebuildIndex
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
	// IndexDecl's. Caller's recovery path is Tx.RebuildIndex (per
	// indexing.md §Drift Guard + §Rebuild).
	ErrIndexFingerprintMismatch = errors.New("gmdb: index fingerprint mismatch — RebuildIndex required")

	// ErrIndexUniqueViolation is returned by Put / Cursor.Delete on
	// an indexed keyspace when a unique-index probe detects a
	// duplicate index key — either against the existing on-disk
	// index, or within the candidate-set produced by a single
	// extractor invocation. Neither the row nor any index entries
	// are written. Per indexing.md §Unique Indexes.
	ErrIndexUniqueViolation = errors.New("gmdb: unique index violation")

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
	// registered on the keyspace, and by Tx.DropIndex /
	// Tx.RebuildIndex when the index name does not match any
	// registry entry on the keyspace. Distinct from ErrNotFound to
	// let callers dispatch between keyspace-missing
	// (ErrNotFound — keyspace-management dimension) and
	// index-name-missing (ErrIndexNotFound — index-management
	// dimension) per chunk-7.1 user-locked.
	ErrIndexNotFound = errors.New("gmdb: index not found")

	// ErrIndexEncoderIDEmpty is returned by TypedIndex declaration
	// when the supplied Encoder[T].ID() returns "" — encoder IDs
	// must be unique non-empty strings for schema-hash
	// determinism. Per typed-keyspaces.md (lands at chunk 9).
	ErrIndexEncoderIDEmpty = errors.New("gmdb: typed index encoder returned empty ID() — encoder IDs must be unique non-empty strings")

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
	errInvalidPageSize           = errors.New("gmdb: PageSize must be a power of two in [4096, 65536]")
	errInvalidSizeBounds         = errors.New("gmdb: MaxSize must be > 0 and >= MinSize")
	errInvalidTxBuffer           = errors.New("gmdb: MaxTxBufferBytes must be > 0")
	errInvalidMaxReaders         = errors.New("gmdb: MaxReaders must be in [1, 65536]")
	errInvalidSyncMode           = errors.New("gmdb: SyncMode out of range")
	errSyncUnsafeRequiresOptIn   = errors.New("gmdb: SyncUnsafe requires AllowSyncUnsafe=true")
	errInvalidMergeThreshold     = errors.New("gmdb: MergeThreshold must be in [1, 50]")
	errInvalidRestartGroupTarget = errors.New("gmdb: RestartGroupTarget must be in [0, 255]")
)

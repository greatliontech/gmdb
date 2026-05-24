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
)

var (
	errInvalidPageSize         = errors.New("gmdb: PageSize must be a power of two in [4096, 65536]")
	errInvalidSizeBounds       = errors.New("gmdb: MaxSize must be > 0 and >= MinSize")
	errInvalidTxBuffer         = errors.New("gmdb: MaxTxBufferBytes must be > 0")
	errInvalidMaxReaders       = errors.New("gmdb: MaxReaders must be in [1, 65536]")
	errInvalidSyncMode         = errors.New("gmdb: SyncMode out of range")
	errSyncUnsafeRequiresOptIn = errors.New("gmdb: SyncUnsafe requires AllowSyncUnsafe=true")
)

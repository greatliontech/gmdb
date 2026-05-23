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
)

var (
	errInvalidPageSize   = errors.New("gmdb: PageSize must be a power of two in [4096, 65536]")
	errInvalidSizeBounds = errors.New("gmdb: MaxSize must be > 0 and >= MinSize")
	errInvalidTxBuffer   = errors.New("gmdb: MaxTxBufferBytes must be > 0")
	errInvalidMaxReaders = errors.New("gmdb: MaxReaders must be in [1, 65536]")
)

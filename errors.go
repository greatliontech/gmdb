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
)

var (
	errInvalidPageSize   = errors.New("gmdb: PageSize must be a power of two in [4096, 65536]")
	errInvalidSizeBounds = errors.New("gmdb: MaxSize must be > 0 and >= MinSize")
	errInvalidTxBuffer   = errors.New("gmdb: MaxTxBufferBytes must be > 0")
)

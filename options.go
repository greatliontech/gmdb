// Package gmdb is a memory-mapped, multi-process, embedded key-value
// database for Go 1.24+.
//
// This file scopes Options to the chunk-1 acceptance surface only: page
// size, checksum, file-size bounds, and the slab budget. The full
// option set in api-surface.md lands incrementally as later chunks
// require it (SyncMode, MaxReaders, RestartGroupTarget, etc.).
package gmdb

import (
	"cmp"
	"crypto/rand"
	"log/slog"

	"github.com/thegrumpylion/gmdb/internal/lock"
	"github.com/thegrumpylion/gmdb/internal/page"
)

// SyncMode controls the durability guarantees of committed
// transactions. Per durability.md §Durability Modes:
//
//   - SyncDurable (default): full ACID. fdatasync at commit step 2
//     (data + RPL + bitmap) AND step 4 (meta). Slowest.
//   - SyncDataOnly: fdatasync at step 2; skip step 4. Last txn may
//     be lost on crash; DB stays consistent (falls back to previous
//     meta). ~2× faster than SyncDurable.
//   - SyncLazy: skip both syncs. Recovery rolls back to the last
//     `Checkpoint()`. DB is always consistent (no corruption).
//   - SyncUnsafe: skip both syncs, no safety net. Requires explicit
//     AllowSyncUnsafe=true. Risk of corruption on crash; benchmarks
//     and ephemeral data only.
//
// SyncMode is a per-process option, not persisted on disk —
// different processes attached to the same database may use
// different SyncModes. The on-disk checkpoint flag reflects
// whichever mode the committer used (durability.md §Cross-process
// SyncMode interleaving).
type SyncMode int

const (
	SyncDurable  SyncMode = iota // syncs data + meta. Full ACID. Default.
	SyncDataOnly                 // syncs data; not meta. Last txn may be lost on crash.
	SyncLazy                     // skips all syncs. Rolls back to last Checkpoint() on crash.
	SyncUnsafe                   // skips all syncs, no safety net. Requires AllowSyncUnsafe.
)

// LaggingReaderInfo carries the diagnostic context the
// Options.LaggingReader callback needs to decide whether to wait or
// abort. Per lock-ordering.md §Lagging Reader Handling, the engine
// constructs this from the reader table + the in-memory RPL state at
// the moment AllocPage detects that bitmap and RPL reclamation are
// both exhausted and a specific reader is the blocking factor.
type LaggingReaderInfo struct {
	// PID is the process ID of the blocking reader (the process that
	// holds the oldest in-progress read tx whose TxnID gates RPL
	// reclamation).
	PID uint32

	// TxnID is the reader's snapshot transaction ID — the value that
	// determines how far the RPL reclamation bound can advance.
	TxnID uint64

	// Lag is the difference between the current write-tx TxnID and
	// the reader's TxnID. Larger values indicate the reader has been
	// pinned longer; callers can use this as a coarse "is this
	// reader hung?" signal.
	Lag uint64

	// HeldPages estimates the page count the reader is pinning
	// unreclaimable in the RPL (count of RPL entries with this
	// reader's TxnID, plus the segment-page overhead). Zero if the
	// engine cannot cheaply derive a count.
	HeldPages uint64
}

// LaggingReaderAction is the return type of the Options.LaggingReader
// callback per lock-ordering.md §Lagging Reader Handling.
type LaggingReaderAction int

const (
	// LaggingReaderWait directs AllocPage to refresh the reader table
	// and retry reclamation. Use this when the blocking reader is
	// expected to commit/rollback soon (interactive workload).
	LaggingReaderWait LaggingReaderAction = iota

	// LaggingReaderAbort directs AllocPage to return ErrDBFull
	// immediately, surfacing the back-pressure to the writer. Use
	// this when the blocking reader is suspected hung or when the
	// caller wants deterministic failure rather than indefinite
	// retry.
	LaggingReaderAbort
)

// Options configures a fresh database at creation time. For an existing
// database the persisted meta is authoritative; Options is consulted
// only for the runtime fields (MaxTxBufferBytes, ReadOnly) that have no
// on-disk counterpart.
type Options struct {
	// PageSize is set at creation, immutable afterwards. Must be a
	// power of two in [4 KB, 64 KB]. Default 4096.
	PageSize uint32

	// PageChecksum enables the xxhash64 page-footer on every data
	// page. Set at creation, immutable. Default true.
	PageChecksum bool

	// MinSize, MaxSize, GrowStep, ShrinkThreshold control file
	// growth and shrinkage in pages. MaxSize is immutable after
	// creation. Defaults: MinSize=64, MaxSize=4_194_304 (16 GiB at
	// 4 KB), GrowStep=64, ShrinkThreshold=128.
	MinSize         uint64
	MaxSize         uint64
	GrowStep        uint64
	ShrinkThreshold uint64

	// MaxTxBufferBytes bounds the per-transaction slab. Exceeding
	// this returns ErrTxTooLarge. Default 256 MiB.
	MaxTxBufferBytes int

	// MaxReaders is the reader-table capacity in the lock file
	// (cross-process.md §Lock File Layout). Set at lock-file creation
	// and immutable afterwards (re-openers honour the on-disk header).
	// Default 4096. Bounded [1, 65536] by the lock package.
	MaxReaders uint32

	// UUID may be supplied for deterministic database identity in
	// tests; if zero, a random UUID is generated at creation.
	UUID [16]byte

	// SyncMode controls per-commit durability — see SyncMode
	// constants. Zero value is SyncDurable (full ACID, the default).
	// Per-process, not persisted; cross-process composition uses the
	// on-disk MetaFlagCheckpoint to coordinate recovery.
	SyncMode SyncMode

	// AllowSyncUnsafe must be true when SyncMode == SyncUnsafe.
	// Without it, Open returns ErrInvalidOptions per durability.md
	// §SyncUnsafe Warning — "a silently-enabled SyncUnsafe lets a
	// benchmark configuration leak into a production deploy."
	AllowSyncUnsafe bool

	// RestartGroupTarget is the engine-wide default for the leaf
	// restart-group target — the maximum entries per group on
	// compressed leaves. Per-keyspace overrides via
	// Tx.SetKeyspaceConfig. Bounded to [0, 255]: 0 ⇒ engine default
	// (16); 1 ⇒ uncompressed leaf variant; [2, 255] ⇒ compressed
	// with that target. Open rejects values > 255 with
	// ErrInvalidOptions — the compressed-leaf restart-table Count
	// is uint8. Default: 16.
	RestartGroupTarget uint16

	// MergeThreshold is the B+tree page fill percentage below which
	// a page is merged with a sibling after deletion. Range: 1-50.
	// Default: 25 (DefaultMergeThreshold). Above 50% redistribute
	// thrash becomes pathological — two siblings each hovering just
	// below 50% would never have room to merge.
	MergeThreshold uint8

	// LaggingReader is called by pager.AllocPage when bitmap +
	// RPL-reclamation are both exhausted AND a specific reader is
	// the blocking factor for further RPL advance, per
	// lock-ordering.md §Lagging Reader Handling. The callback decides
	// Wait (refresh + retry) or Abort (ErrDBFull). Invoked at most
	// once per AllocPage call (lock-ordering.md invariant) to avoid
	// busy loops. Nil ⇒ AllocPage falls through to file extension
	// when reclamation is blocked.
	LaggingReader func(info LaggingReaderInfo) LaggingReaderAction

	// Logger receives diagnostic messages — leak detection warnings,
	// non-fatal recovery events. Default: nil = discard. Set to
	// slog.Default() to route to the process-wide handler, or to a
	// custom *slog.Logger for structured per-DB logging.
	Logger *slog.Logger
}

func (o Options) applyDefaults() Options {
	o.PageSize = cmp.Or(o.PageSize, uint32(4096))
	o.MinSize = cmp.Or(o.MinSize, uint64(64))
	o.MaxSize = cmp.Or(o.MaxSize, uint64(4_194_304))
	o.GrowStep = cmp.Or(o.GrowStep, uint64(64))
	o.ShrinkThreshold = cmp.Or(o.ShrinkThreshold, uint64(128))
	o.MaxTxBufferBytes = cmp.Or(o.MaxTxBufferBytes, 256<<20)
	o.MaxReaders = cmp.Or(o.MaxReaders, lock.DefaultMaxReaders)
	o.MergeThreshold = cmp.Or(o.MergeThreshold, defaultMergeThreshold)
	// RestartGroupTarget defaults to 0 (engine default at the leaf
	// codec layer — currently 16 per page-formats.md §Compressed
	// Leaf). 0 is the canonical "use engine default" sentinel.
	if o.UUID == ([16]byte{}) {
		_, _ = rand.Read(o.UUID[:])
	}
	return o
}

const defaultMergeThreshold uint8 = 25
const maxMergeThreshold uint8 = 50

func (o Options) validate() error {
	if !page.ValidPageSize(o.PageSize) {
		return errInvalidPageSize
	}
	if o.MaxSize == 0 || o.MinSize > o.MaxSize {
		return errInvalidSizeBounds
	}
	if o.MaxTxBufferBytes <= 0 {
		return errInvalidTxBuffer
	}
	// Pre-check the lock-package's MaxReaders bound so an
	// out-of-range value fails Open before the data file is touched,
	// rather than after pager.Open + lock.Open. The lock package
	// re-validates as defense-in-depth (mapLockErr's
	// ErrInvalidMaxReaders branch handles the late failure path).
	if o.MaxReaders < lock.MinMaxReaders || o.MaxReaders > lock.MaxMaxReaders {
		return errInvalidMaxReaders
	}
	switch o.SyncMode {
	case SyncDurable, SyncDataOnly, SyncLazy:
		// All safe modes.
	case SyncUnsafe:
		if !o.AllowSyncUnsafe {
			return errSyncUnsafeRequiresOptIn
		}
	default:
		return errInvalidSyncMode
	}
	if o.MergeThreshold < 1 || o.MergeThreshold > maxMergeThreshold {
		return errInvalidMergeThreshold
	}
	if o.RestartGroupTarget > page.MaxRestartGroupTarget {
		return errInvalidRestartGroupTarget
	}
	return nil
}

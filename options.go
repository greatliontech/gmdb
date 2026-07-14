// Package gmdb is a memory-mapped, multi-process, embedded key-value
// database for Go 1.24+.
//
// This file holds Options and its validation; the option surface
// follows api-surface.md §Options.
package gmdb

import (
	"cmp"
	"crypto/rand"
	"log/slog"
	"os"
	"time"

	"github.com/greatliontech/gmdb/internal/lock"
	"github.com/greatliontech/gmdb/internal/page"
)

// SyncMode controls the durability guarantees of committed
// transactions. Per durability.md §Durability Modes:
//
//   - SyncDurable (default): full ACID. fdatasync at commit step 2
//     (data + RPL + bitmap) AND step 4 (meta). Slowest.
//   - SyncDataOnly: fdatasync at step 2; skip step 4. Last txn may
//     be lost on crash; DB stays consistent (falls back to previous
//     meta). ~2× faster than SyncDurable.
//   - SyncLazy: skip both syncs. Crash recovery rolls back to the
//     durable epoch (the last fsync point by any handle). DB is
//     always consistent (no corruption).
//
// SyncMode is a per-process option, not persisted on disk —
// different processes attached to the same database may use
// different SyncModes. The meta's durable sub-record reflects
// whichever fsync-ing event happened last (durability.md
// §Cross-process SyncMode interleaving).
type SyncMode int

const (
	SyncDurable  SyncMode = iota // syncs data + meta. Full ACID. Default.
	SyncDataOnly                 // syncs data; not meta. Last txn may be lost on crash.
	SyncLazy                     // skips all syncs. Rolls back to the durable epoch on crash.
)

// LeafLayout selects a compressed-leaf layout variant
// (page-formats.md §Leaf Page). Numeric values match the keyspace
// descriptor's NodeLayouts field encoding (keyspaces.md).
type LeafLayout uint8

const (
	// LeafLayoutDefault defers to the engine default (segregated).
	LeafLayoutDefault LeafLayout = 0
	// LeafLayoutInterleaved stores each entry's value bytes after its
	// key bytes in one stream — favors in-place value splices on
	// write-heavy keyspaces.
	LeafLayoutInterleaved LeafLayout = 1
	// LeafLayoutSegregated packs entry headers + key bytes as a pure
	// search region and value bytes in a separate region — the
	// engine default.
	LeafLayoutSegregated LeafLayout = 2
)

// BranchLayout selects a branch layout variant (page-formats.md
// §Branch Page). Numeric values match the descriptor encoding.
type BranchLayout uint8

const (
	// BranchLayoutDefault defers to the engine default (segregated).
	BranchLayoutDefault BranchLayout = 0
	// BranchLayoutPlain stores full separator bytes per cell — the
	// probe-latency floor for small hot keyspaces.
	BranchLayoutPlain BranchLayout = 1
	// BranchLayoutSegregated stores the page's shared separator
	// prefix once with an offsets-only suffix directory — the
	// density choice for large trees.
	BranchLayoutSegregated BranchLayout = 2
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

	// DisablePageChecksum turns OFF the XXH3-64 page-footer that is
	// otherwise written and verified on every data page. Set at
	// creation, immutable for the life of the file. The zero value
	// (false) leaves checksums ENABLED — the spec default (see
	// checksums.md §Data Page Checksums): opt out only on a filesystem
	// or controller with its own end-to-end integrity (ZFS, btrfs,
	// ReFS) where the 0.2% page-space saving is worth losing bitrot
	// detection.
	DisablePageChecksum bool

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

	// ReadOnly opens the database read-only: the writer pager path is
	// not initialised, no background maintenance runs, and the write
	// entry points (Begin / Update / Batch / Compact / Checkpoint)
	// return ErrDatabaseReadOnly. Reads (BeginRead / View) work
	// normally; the data mmap is always PROT_READ regardless.
	//
	// A read-only handle still pins reader slots so a concurrent
	// cross-process writer cannot reclaim pages under an in-flight
	// read: when the lock file can be opened read-write, Open starts
	// the heartbeat goroutine and read transactions acquire slots as
	// usual — only the writer flock-grant goroutine is skipped. On
	// truly read-only media (lock file not openable read-write), Open
	// falls back to lock-free snapshot reads and logs a warning
	// (mmap-strategy.md §Read-Only). When ReadOnly is set and the file
	// does not exist, Open returns os.ErrNotExist — it never creates a
	// database read-only. Default: false.
	ReadOnly bool

	// PreloadPages calls madvise(MADV_POPULATE_READ) on the
	// file-backed portion of the data mmap (pages 0 through
	// HighWaterMark-1) at open, prefaulting them into the OS page
	// cache so the first reads don't pay per-page fault costs
	// (mmap-strategy.md §Prefaulting). Linux 5.14+; silent no-op on
	// older kernels and non-Linux. Default: false (demand paging).
	PreloadPages bool

	// HugePages calls madvise(MADV_HUGEPAGE) on the data mmap at open,
	// enabling transparent-huge-page backing to cut TLB pressure for
	// large databases (mmap-strategy.md §Huge Pages). Linux; silent
	// no-op on non-Linux and on kernels without THP for file-backed
	// mappings. Default: false.
	HugePages bool

	// ReclaimOnClose calls madvise(MADV_COLD) over the page range a
	// read transaction touched when it closes, hinting the kernel it
	// may reclaim those pages under memory pressure — useful for large
	// sequential scans (exports, analytics) that would otherwise evict
	// hotter pages (mmap-strategy.md §Read Transaction Cooldown). Linux
	// 5.4+; silent no-op on older kernels and non-Linux. Enabling it
	// costs two atomic min/max updates per page read. Default: false.
	ReclaimOnClose bool

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
	// Per-process, not persisted; cross-process composition rides the
	// meta's durable sub-record (durability.md).
	SyncMode SyncMode

	// RestartGroupTarget is the engine-wide default for the leaf
	// restart-group target — the maximum entries per group on
	// compressed leaves. Per-keyspace overrides via
	// Tx.SetKeyspaceConfig. Bounded to [0, 255]: 0 ⇒ engine default
	// (6); 1 ⇒ uncompressed leaf variant; [2, 255] ⇒ compressed
	// with that target. Open rejects values > 255 with
	// ErrInvalidOptions — the compressed-leaf restart-table Count
	// is uint8. Default: 6.
	RestartGroupTarget uint16

	// LeafLayout and BranchLayout are the engine-wide default node
	// layout variants for keyspaces whose descriptor declares the
	// engine default (keyspaces.md §Keyspace Descriptor NodeLayouts).
	// Zero values take the engine defaults (segregated). Per-OPEN,
	// not persisted (unlike DisablePageChecksum): the opening
	// process's value resolves descriptor-0 keyspaces at page-build
	// time, the same semantics as RestartGroupTarget — per-page
	// type-byte dispatch makes differing defaults across opens yield
	// mixed-layout pages, never misreads. Per-keyspace overrides via
	// Tx.SetKeyspaceConfig. Open rejects unknown values with
	// ErrInvalidOptions.
	LeafLayout   LeafLayout
	BranchLayout BranchLayout

	// MergeThreshold is the B+tree page fill percentage that doubles
	// as the post-deletion merge **trigger** AND the maintained
	// non-root fill **floor**. Range: 1-50. Default: 25
	// (DefaultMergeThreshold).
	//
	// Trigger: after a `Delete` or `DeleteRange` mutation drops a
	// page below MergeThreshold% of its `ContentEnd`, the page is
	// merged with (or redistributed against) an adjacent sibling.
	//
	// Floor: after a successful `Delete` or `DeleteRange` returns,
	// every non-root page reachable from the new root has encoded
	// fill `>= MergeThreshold%` of `ContentEnd` — see
	// `range-delete.md §Invariants` for the full fill-floor clause
	// (including the post-merge re-rebalance loop and the
	// cousin-cascade thread that close the 2-survivor edge case).
	// The root is exempt: a partially-emptied tree's root may be
	// arbitrarily small or root-collapse to empty.
	//
	// Above 50% redistribute thrash becomes pathological — two
	// siblings each hovering just below 50% would never have room
	// to merge, and each Delete would force a redistribute.
	MergeThreshold uint8

	// LaggingReader is called by pager.AllocPage when bitmap +
	// RPL-reclamation are both exhausted AND a specific reader is
	// the blocking factor for further RPL advance, per
	// lock-ordering.md §Lagging Reader Handling. The callback decides
	// Wait (refresh + retry) or Abort (ErrDBFull). Invoked at most
	// once per AllocPage call (lock-ordering.md invariant) to avoid
	// busy loops. Nil ⇒ AllocPage falls through to file extension
	// when reclamation is blocked.
	//
	// REENTRANCY: the callback runs on the writer's goroutine WHILE
	// the write grant is held. It must not call any gmdb write entry
	// point (Begin/Update/Batch/Checkpoint/Compact) on the same DB —
	// the call queues behind its own grant and, with a no-deadline
	// context, deadlocks permanently. Signal another goroutine for
	// corrective action instead (lock-ordering.md §Lagging Reader
	// Handling).
	//
	// REACHABILITY: the engine retains this callback for the handle's
	// lifetime; anything it captures — including the *DB itself —
	// stays reachable, making handle-leak detection inert for such a
	// handle (leak-detection.md §Database Handle Leak Detection).
	LaggingReader func(info LaggingReaderInfo) LaggingReaderAction

	// Logger receives diagnostic messages — leak detection warnings,
	// non-fatal recovery events. Default: nil = discard. Set to
	// slog.Default() to route to the process-wide handler, or to a
	// custom *slog.Logger for structured per-DB logging.
	//
	// REACHABILITY: the engine retains the logger (including in leak-
	// cleanup state) for the handle's lifetime; a handler capturing
	// the *DB keeps it reachable and makes handle-leak detection
	// inert for that handle (leak-detection.md §Database Handle Leak
	// Detection).
	Logger *slog.Logger

	// ScratchDir is the directory used for BulkLoad sort spill on
	// indexed keyspaces — the external merge-sort writes spill chunks
	// here when the per-index entry set exceeds MaxTxBufferBytes. Must
	// be on the same filesystem as the database file when Compact() is
	// used (atomic rename requirement). Default: os.TempDir(). Per
	// api-surface.md §Options + bulkload.md §Interaction with Indexes.
	ScratchDir string

	// MaxBatchSize is the maximum number of DB.Batch calls the
	// coordinator collects into one write transaction before executing.
	// Default: 1000. Set to 1 to disable batching (each Batch call runs
	// in its own transaction). Per transactions.md §Write Batching.
	MaxBatchSize int

	// MaxBatchDelay is the maximum time the coordinator waits for
	// additional Batch calls after the first call in a batch before
	// executing. Lower → lower latency; higher → higher throughput.
	// Default: 10ms. (The zero value takes the default; to fire with
	// minimal waiting use a small delay or MaxBatchSize = 1.) Per
	// transactions.md §Write Batching.
	MaxBatchDelay time.Duration

	// StaleTimeout is how long a reader slot's (or a peer writer's)
	// heartbeat may lag this process's monotonic clock before
	// cross-process stale-detection reclaims it (cross-process.md
	// §Heartbeat Goroutine). It is a *data-integrity* bound, not a
	// performance knob: a window shorter than a live peer's effective
	// heartbeat cadence lets a writer's RPL-reclamation scan evict that
	// peer's still-live reader and reclaim pages it is still reading.
	// Must be significantly larger than HeartbeatInterval to absorb
	// scheduling jitter; Open rejects StaleTimeout <= HeartbeatInterval
	// with ErrInvalidOptions. Zero ⇒ default. Default: 10s.
	StaleTimeout time.Duration

	// CrossNamespaceStaleTimeout governs every CROSS-namespace (or
	// namespace-unknown) liveness classification — container peers,
	// where the heartbeat is the only signal and a paused or frozen
	// container (docker pause, cgroup freeze, heavy swap) stops
	// heartbeating while its reads stay live (cross-process.md
	// §Stale-reader detection, cross-namespace window). Same-namespace
	// peers are classified by kill(0) + process start time and are
	// unaffected. Zero ⇒ 6 × the effective StaleTimeout (60 s at
	// defaults). Must be >= StaleTimeout. The trade: a genuinely dead
	// container pins RPL reclamation for this window instead of
	// StaleTimeout.
	CrossNamespaceStaleTimeout time.Duration

	// HeartbeatInterval is how often the per-DB heartbeat goroutine
	// refreshes WriterHeartbeat (while this process holds the write
	// lock) and the Heartbeat field of every active reader slot
	// (cross-process.md §Heartbeat Goroutine). Must be well under
	// StaleTimeout so a few missed ticks don't trip false-stale
	// detection. Zero ⇒ default. Default: 1s.
	HeartbeatInterval time.Duration

	// LockRetryInterval is the polling tick the flock goroutine uses
	// when flock(LOCK_EX|LOCK_NB) returns EWOULDBLOCK under
	// cross-process write-lock contention (cross-process.md §Write
	// Lock). It bounds both Close() shutdown latency and per-writer ctx
	// cancellation latency while another process holds the lock — one
	// wasted non-blocking flock syscall per tick. Zero ⇒ default.
	// Default: 50ms.
	LockRetryInterval time.Duration

	// CompactDrainTimeout bounds how long Compact() waits for active
	// in-process read transactions to commit/rollback before aborting
	// with ErrCompactReadersActive. Default: 30s. Per api-surface.md
	// §Compact. (Cross-process readers are not drained — they continue
	// against the pre-Compact inode.)
	CompactDrainTimeout time.Duration

	// Maintenance configures the background maintenance goroutine
	// (background-maintenance.md). The zero value enables maintenance
	// with all defaults.
	Maintenance MaintenanceOptions
}

// MaintenanceOptions configures the background maintenance goroutine
// (background-maintenance.md §Options).
type MaintenanceOptions struct {
	// Disable disables the background maintenance goroutine entirely.
	// Default: false (maintenance enabled).
	Disable bool

	// Interval is the minimum time between maintenance passes,
	// coordinated across processes via the lock file's
	// LastMaintenanceTime. Default: 5m.
	Interval time.Duration

	// ScrubBatchSize is the number of pages verified per checksum-
	// scrubbing pass (only meaningful when page checksums are enabled,
	// i.e. Options.DisablePageChecksum is false). Default: 4096.
	ScrubBatchSize int

	// CompactionThreshold is the contiguous-allocation failure rate above
	// which incremental compaction triggers — the fraction of multi-page
	// allocations whose first bitmap scan finds no contiguous run despite
	// sufficient total free pages. Range [0,1]: 0 is most aggressive
	// (compact on any fragmentation), 1 is least (effectively never).
	// Default: 0.5. To disable compaction specifically while keeping the
	// other maintenance tasks, set DisableCompaction.
	CompactionThreshold float64

	// CompactionBatchSize is the number of pages relocated per
	// incremental-compaction write transaction. Default: 1024.
	CompactionBatchSize int

	// DisableCompaction turns off incremental compaction (Task 4) only,
	// leaving bitmap leak reclamation, stale-reader cleanup, and checksum
	// scrubbing running. Compaction is the one maintenance task that
	// rewrites live data (the CoW relocation cascade) and amplifies
	// writes, so a write-amplification- or space-sensitive deployment may
	// want it off while keeping the cheap, safety-relevant tasks (leak
	// reclamation in particular). Default: false (compaction enabled).
	// To disable ALL maintenance instead, use Disable.
	DisableCompaction bool
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
	o.ScratchDir = cmp.Or(o.ScratchDir, os.TempDir())
	o.MaxBatchSize = cmp.Or(o.MaxBatchSize, defaultMaxBatchSize)
	o.MaxBatchDelay = cmp.Or(o.MaxBatchDelay, defaultMaxBatchDelay)
	// The cross-process coordination intervals share the lock package's
	// own defaults as the single source of truth (NewCoord applies the
	// same zero⇒default fallback as defense-in-depth). Resolving them
	// here lets validate() check the StaleTimeout > HeartbeatInterval
	// relation against effective values rather than raw zeros.
	o.StaleTimeout = cmp.Or(o.StaleTimeout, lock.DefaultStaleTimeout)
	o.CrossNamespaceStaleTimeout = cmp.Or(o.CrossNamespaceStaleTimeout, 6*o.StaleTimeout)
	o.HeartbeatInterval = cmp.Or(o.HeartbeatInterval, lock.DefaultHeartbeatInterval)
	o.LockRetryInterval = cmp.Or(o.LockRetryInterval, lock.DefaultRetryInterval)
	o.CompactDrainTimeout = cmp.Or(o.CompactDrainTimeout, defaultCompactDrainTimeout)
	o.Maintenance.Interval = cmp.Or(o.Maintenance.Interval, defaultMaintenanceInterval)
	o.Maintenance.ScrubBatchSize = cmp.Or(o.Maintenance.ScrubBatchSize, defaultScrubBatchSize)
	o.Maintenance.CompactionThreshold = cmp.Or(o.Maintenance.CompactionThreshold, defaultCompactionThreshold)
	o.Maintenance.CompactionBatchSize = cmp.Or(o.Maintenance.CompactionBatchSize, defaultCompactionBatchSize)
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

const (
	defaultMaxBatchSize        = 1000
	defaultMaxBatchDelay       = 10 * time.Millisecond
	defaultCompactDrainTimeout = 30 * time.Second
	defaultMaintenanceInterval = 5 * time.Minute
	defaultScrubBatchSize      = 4096
	defaultCompactionThreshold = 0.5
	defaultCompactionBatchSize = 1024
)

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
	default:
		return errInvalidSyncMode
	}
	if o.MergeThreshold < 1 || o.MergeThreshold > maxMergeThreshold {
		return errInvalidMergeThreshold
	}
	if o.RestartGroupTarget > page.MaxRestartGroupTarget {
		return errInvalidRestartGroupTarget
	}
	if o.LeafLayout > LeafLayoutSegregated {
		return errInvalidLeafLayout
	}
	if o.BranchLayout > BranchLayoutSegregated {
		return errInvalidBranchLayout
	}
	// Validated after applyDefaults: a zero MaxBatchSize/MaxBatchDelay has
	// already been replaced by its default, so only an explicit negative
	// (which cmp.Or leaves untouched) reaches here.
	if o.MaxBatchSize < 1 {
		return errInvalidMaxBatchSize
	}
	if o.MaxBatchDelay < 0 {
		return errInvalidMaxBatchDelay
	}
	// Cross-process coordination intervals: validated after
	// applyDefaults, so a zero is already the default and only an
	// explicit negative (which cmp.Or leaves untouched) reaches here.
	if o.StaleTimeout < 0 || o.HeartbeatInterval < 0 || o.LockRetryInterval < 0 {
		return errInvalidCoordInterval
	}
	// StaleTimeout > HeartbeatInterval is a data-integrity precondition,
	// not a tuning preference (cross-process.md §Heartbeat Goroutine:
	// "Must be significantly larger than the heartbeat interval ... for
	// scheduling jitter"). At or below the heartbeat cadence, a single
	// jitter-delayed tick lets a writer's OldestReaderTxnID scan
	// misclassify a live reader slot as stale and reclaim pages it is
	// still reading (use-after-reclaim). Reject the always-unsafe region
	// at Open; the godoc recommends a window several times larger.
	if o.StaleTimeout <= o.HeartbeatInterval {
		return errStaleTimeoutTooSmall
	}
	// The cross-namespace window can never be TIGHTER than the general
	// one — it exists to widen the heartbeat-only classification for
	// freezable container peers (cross-process.md §Stale-reader
	// detection, cross-namespace window).
	if o.CrossNamespaceStaleTimeout < o.StaleTimeout {
		return errCrossNSStaleTimeoutTooSmall
	}
	// Maintenance: validated after applyDefaults (a zero Interval /
	// ScrubBatchSize / CompactionBatchSize / CompactionThreshold is already
	// the default), so only an explicit negative or out-of-range value
	// reaches here. A negative Interval would panic time.NewTicker inside
	// the maintenance goroutine (no recover) — reject at Open.
	if o.Maintenance.Interval < 0 ||
		o.Maintenance.ScrubBatchSize < 0 ||
		o.Maintenance.CompactionBatchSize < 0 ||
		o.Maintenance.CompactionThreshold < 0 || o.Maintenance.CompactionThreshold > 1 {
		return errInvalidMaintenance
	}
	return nil
}

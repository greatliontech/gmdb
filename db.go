package gmdb

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
	"weak"

	"github.com/greatliontech/gmdb/internal/closegate"
	"github.com/greatliontech/gmdb/internal/lock"
	"github.com/greatliontech/gmdb/internal/page"
	"github.com/greatliontech/gmdb/internal/pager"
)

// DB is a handle to an open gmdb database. Concurrent reads + a single
// in-process / cross-process writer are coordinated via the lock file
// (cross-process.md). Close() drains coordination goroutines and
// releases all mappings.
type DB struct {
	file *os.File
	root *os.Root // path-traversal guard from os.OpenRoot
	path string   // the data-file path as passed to Open (for Compact's temp + rename)

	pool *pager.BufPool
	opts Options

	// logger captures Options.Logger at Open with the
	// nil → discard-handler fallback (the default: a nil
	// Options.Logger discards diagnostic output rather than routing
	// to slog.Default()). Cleanup paths reference this via cleanup-
	// closure captures rather than slog.Default() so per-DB logging
	// is honored even after the *DB is collected.
	logger *slog.Logger

	// Cross-process coordination. lockFile is the mmap'd lock file
	// (cross-process.md §Lock File Layout); coord owns the flock +
	// heartbeat goroutines and is the only writer to the writer-
	// header fields. coord is constructed in Open and torn down by
	// Close — its lifetime is the DB handle's lifetime.
	lockFile *lock.File
	coord    *lock.Coord

	// readOnly is set when the DB was opened with Options.ReadOnly. The
	// writer pager path is never built; the write entry points (Begin /
	// Update / Batch / Compact / Checkpoint) reject with
	// ErrDatabaseReadOnly. When the lock file could not be opened
	// read-write (read-only media), coord and lockFile are nil and the
	// read path runs lock-free (no reader-slot pinning). Immutable after
	// Open.
	readOnly bool

	// Pager state for the currently-active reader baseline. mu guards
	// against concurrent Begin from multiple goroutines.
	mu            sync.Mutex
	currentMeta   pager.Meta
	activeMetaIdx int
	pgr           *pager.Pager

	// poisoned is set when a write tx's commit failed past the
	// publication boundary (step-3 pwrite or step-4 fdatasync). The
	// on-disk active meta may have advanced while the pager's
	// in-memory state was rolled back by AbortTx — bitmap /
	// HighWaterMark / RPL chain disagree with disk, and any further
	// allocation off this handle could overwrite pages the on-disk
	// tree references. Subsequent Begin returns ErrPoisoned. Close +
	// re-Open is the recovery path: the new handle reads everything
	// from disk and is internally consistent.
	poisoned atomic.Bool

	// dataGeneration caches the lock header's data-file replacement
	// counter as of Open (or this handle's own Compact). Compared
	// against the live header after every write-grant acquisition and
	// reader-slot publish (cross-process.md §Data-file generation): a
	// mismatch means a peer's Compact replaced the inode this handle
	// still maps — poison, Close + re-Open converges.
	dataGeneration atomic.Uint64
	// takeoverSeqSeen is the lock header's takeover sequence as of
	// this handle's last full bitmap+RPL (re)build from the on-disk
	// image — Open's attach, or a forced grant re-sync. Written at
	// Open before the handle escapes, then only under db.mu + the
	// write grant (resyncPagerLocked).
	takeoverSeqSeen uint32

	// closeGate is a heap-allocated coordination struct shared by
	// pointer with every txCleanupInfo, every readTxCleanupInfo, and
	// the dbCleanupInfo. Composes the (closed bool, txInflight
	// counter) pair that leak-detection.md
	// §Cleanup Behavior + §Close Ordering requires — see internal/closegate
	// for the full rationale.
	//
	// Heap allocation is required because runtime.AddCleanup provides
	// no ordering between the DB cleanup and the Tx cleanups that
	// depend on observing the close state; an inline DB field would
	// dangle if the DB were collected first.
	//
	// Close() stores closed=true (release-store) AND drains
	// txInflight to zero BEFORE unmapping the lock file — so a
	// leaked-Tx cleanup that passed the closed gate cannot race the
	// unmap. The drain is a bounded spin (cleanup work is two
	// atomic stores).
	closeGate *closegate.Gate

	// cleanup is the runtime.AddCleanup handle for THIS *DB; Stop()'d
	// by Close so a normal teardown doesn't fire the leak-warning
	// path.
	cleanup runtime.Cleanup

	// batch holds the lazily-started Batch() coordinator state; maint
	// holds the background-maintenance goroutine state. See batch.go and
	// maintenance.go for their lifecycles and the types' field godocs.
	batch batchCoordinator
	maint maintenance
}

// pagerOpenParamsFrom is the single Options → pager.OpenParams
// derivation. Open and Compact's pager reopen must hand the pager
// identical per-open configuration (builder defaults included); a
// second hand-rolled construction is how those defaults were once
// silently dropped across Compact.
func pagerOpenParamsFrom(pool *pager.BufPool, opts Options) pager.OpenParams {
	return pager.OpenParams{
		Pool:               pool,
		MaxTxBufferBytes:   opts.MaxTxBufferBytes,
		RestartGroupTarget: opts.RestartGroupTarget,
		LeafLayout:         page.LeafLayout(opts.LeafLayout),
		BranchLayout:       page.BranchLayout(opts.BranchLayout),
		NoFullFsync:        opts.NoFullFsync,
	}
}

// Open opens the database at path. If the file does not exist, it is
// created with opts; existing files use opts for runtime fields only
// (PageSize, PageChecksum, file-size bounds are taken from the meta
// page).
//
// Open uses os.OpenRoot on `filepath.Dir(path)` so that the data
// file's open and any subsequent in-Root operations are confined to
// that directory — a symlink at `path` resolving outside of it is
// rejected by the Go 1.24+ Root semantics. NB: the path itself is not
// canonicalised; a caller that passes `/foo/../etc/passwd` resolves
// `dir = /etc` and opens `passwd` there. The guard prevents
// symlink-escape from `dir`, not directory-traversal in the input.
//
// The context governs cancellation: an already-cancelled or expired
// ctx fails fast before any filesystem work, and cancellation during
// the partial-init retry wait aborts promptly. ctx is kept on the
// signature for future tracing / timeout / cancellation wiring — it
// does not yet abort a syscall already in flight (a stuck fdatasync /
// mmap completes before the next ctx check).
func Open(ctx context.Context, path string, opts Options) (*DB, error) {
	// Check ctx first so an already-cancelled / expired context fails
	// fast before any filesystem work (matches Begin / BeginRead).
	if err := ctx.Err(); err != nil {
		return nil, context.Cause(ctx)
	}
	opts = opts.applyDefaults()
	if err := opts.validate(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidOptions, err)
	}
	// Open-time inode verification retry (cross-process.md §Data-file
	// generation): a peer's Compact can rename the data file between
	// this Open's fd acquisition and its generation-cache read, in
	// which case the handle would cache the post-bump generation
	// while mapping the replaced inode — every later check passing.
	// openAttempt detects the mismatch (fd inode vs path inode,
	// checked AFTER the cache read; the rename happens-before the
	// bump, so either the stat sees the new inode or the per-op check
	// fires) and we retry against the new inode. Three attempts:
	// each retry needs a fresh completed Compact to fail again.
	var lastErr error
	for range 3 {
		db, err := openAttempt(ctx, path, opts)
		if err == nil || !errors.Is(err, errStaleInodeAtOpen) {
			return db, err
		}
		lastErr = err
	}
	return nil, lastErr
}

// errStaleInodeAtOpen is the internal retry signal for the Open-time
// inode verification.
var errStaleInodeAtOpen = errors.New("gmdb: data file replaced during Open")

// shrinkGateHookForTest fires inside the commit-time shrink gate
// after the reader scan passed (no visible readers) and before the
// ftruncate — the exact window the shrink seqlock exists to bracket.
// Tests interleave a BeginRead here to pin the reader-side re-read.
var shrinkGateHookForTest atomic.Pointer[func()]

func openAttempt(ctx context.Context, path string, opts Options) (*DB, error) {
	dir := filepath.Dir(path)
	base := filepath.Base(path)
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("gmdb: open dir %q: %w", dir, err)
	}
	var file *os.File
	created := false
	if opts.ReadOnly {
		// Read-only never creates: open O_RDONLY so the path works on
		// read-only media / read-only filesystem permissions. A missing
		// file surfaces as os.ErrNotExist (wrapped, so errors.Is still
		// matches) per api-surface.md §Options.ReadOnly — opening
		// read-only-then-creating would be a contradiction.
		file, err = root.OpenFile(base, os.O_RDONLY, 0)
		if err != nil {
			_ = root.Close()
			return nil, fmt.Errorf("gmdb: open %q read-only: %w", path, err)
		}
	} else {
		// Per api-surface.md §Database Initialization: try O_CREATE|O_EXCL
		// first and on EEXIST fall back to the normal-open path. This is
		// the correct ordering against a concurrent Open race — the
		// loser observes EEXIST and proceeds as a regular open of the
		// just-created file.
		file, err = root.OpenFile(base, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		created = true
		if err != nil {
			if !errors.Is(err, os.ErrExist) {
				_ = root.Close()
				return nil, fmt.Errorf("gmdb: create %q: %w", path, err)
			}
			created = false
			file, err = root.OpenFile(base, os.O_RDWR, 0o600)
			if err != nil {
				_ = root.Close()
				return nil, fmt.Errorf("gmdb: open %q: %w", path, err)
			}
		}
	}

	if created {
		ip := pager.InitParams{
			PageSize:        opts.PageSize,
			PageChecksum:    !opts.DisablePageChecksum,
			MinSize:         opts.MinSize,
			MaxSize:         opts.MaxSize,
			GrowStep:        opts.GrowStep,
			ShrinkThreshold: opts.ShrinkThreshold,
			UUID:            opts.UUID,
			NoFullFsync:     opts.NoFullFsync,
		}
		if err := pager.Init(file, ip); err != nil {
			_ = file.Close()
			_ = root.Close()
			return nil, fmt.Errorf("gmdb: init: %w", err)
		}
	}

	// Dirent durability (durability.md §Directory-entry durability):
	// pager.Init fsyncs the FILE, but POSIX makes the directory entry
	// durable only after the parent directory is fsynced — without
	// it, power loss after N acked SyncDurable commits can lose the
	// database file entirely. Every WRITABLE Open re-establishes the
	// obligation, not just the creating one: the create-retry after a
	// failed dir fsync and any Open racing a creator that crashed
	// before ITS fsync both land on the EEXIST-fallback path, so a
	// creation-only sync would leave those dirents non-durable
	// forever. One fsync per Open, through the pinned root (symlink
	// guard). Read-only opens skip it (nothing to make durable;
	// read-only media would EROFS). The lock file's dirent needs no
	// such guarantee — transient coordination state Open recreates.
	if !opts.ReadOnly {
		if err := syncDir(root, base, opts.NoFullFsync); err != nil {
			_ = file.Close()
			_ = root.Close()
			return nil, fmt.Errorf("gmdb: open %q: parent directory fsync: %w", path, err)
		}
	}

	// Read the on-disk meta-0 PageSize so the pool matches the
	// persisted size — opts.PageSize is authoritative only at
	// creation; re-opens honour what was written.
	//
	// If we took the EEXIST-retry fallback, another process may still
	// be in the middle of pager.Init (file exists but metas not yet
	// written). Read meta-0 with bounded retry-on-empty so we don't
	// observe a half-initialised file and fail with a misleading
	// "PageSize invalid" error. The lock file resolves the
	// race structurally; this is a defensive
	// measure.
	persistedPageSize, err := readPersistedPageSize(ctx, file, !created)
	if err != nil {
		_ = file.Close()
		_ = root.Close()
		return nil, err
	}
	pool := pager.NewBufPool(int(persistedPageSize))
	pop := pagerOpenParamsFrom(pool, opts)
	var opened *pager.OpenedDB
	if opts.ReadOnly {
		// Read-only handle: build a reader pager (no writer slab / no
		// in-memory bitmap / no RPL chain) — it owns the handle's data
		// mmap. Raw reads (Check, CopyTo, maintenance) go through
		// per-snapshot reader pagers (BeginRead), never this one: its
		// mapping coverage is fixed at open time on windows
		// (mmap-strategy.md §Windows), so a raw read routed here past
		// a peer's growth would fault.
		opened, err = pager.OpenReadOnly(file, pop)
	} else {
		opened, err = pager.Open(file, pop)
	}
	if err != nil {
		_ = file.Close()
		_ = root.Close()
		return nil, mapPagerErr(err)
	}

	// Capture Options.Logger with the default
	// (nil → discard handler) so per-DB logging routes to the
	// caller's chosen sink — never to slog.Default(). Built before the
	// lock section so the read-only fallback can warn through it.
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	// Opt-in mmap tuning on the data mmap at open (mmap-strategy.md
	// §Huge Pages / §Prefaulting). Both are advisory hints: the wrappers
	// swallow "unsupported kernel" (EINVAL/ENOSYS) so an old kernel is a
	// silent no-op, and a real error (e.g. EIO mid-prefault) is logged
	// rather than failing Open — the database is fully usable without
	// the hint. PreloadPages prefaults pages [0, HighWaterMark).
	if opts.HugePages {
		if err := opened.Pager.AdviseHugePages(); err != nil {
			logger.Warn("gmdb: madvise(MADV_HUGEPAGE) failed", "path", path, "detail", err.Error())
		}
	}
	if opts.PreloadPages {
		if err := opened.Pager.AdvisePreload(opened.Meta.HighWaterMark); err != nil {
			logger.Warn("gmdb: madvise(MADV_POPULATE_READ) failed", "path", path, "detail", err.Error())
		}
	}

	// Cache this process's identity once. Failures (no /proc,
	// hardened sandbox, non-Linux ProcessStartTime stub) surface as
	// 0 — the protocol routes through the heartbeat path when either
	// value is unavailable.
	processStartTime, _ := lock.ProcessStartTime(os.Getpid())
	pidNamespace, _ := lock.PIDNamespace()

	// Open the lock file under the same os.Root — symlink-escape
	// protection is shared with the data file. The DataUUID is the
	// just-opened pager's meta UUID; a stale lock file with a
	// different UUID is unlinked-and-recreated by lock.Open.
	var lockFile *lock.File
	var coord *lock.Coord
	lockFile, err = lock.Open(lock.OpenParams{
		Root:       root,
		Base:       lock.BaseFor(base),
		DataUUID:   opened.Meta.UUID,
		MaxReaders: opts.MaxReaders,
	})
	switch {
	case err == nil:
		coord = lock.NewCoord(lockFile, lock.CoordOptions{
			PID:              uint64(os.Getpid()),
			ProcessStartTime: processStartTime,
			PIDNamespace:     pidNamespace,
			// The three cross-process coordination intervals come straight
			// from Options (already resolved to their lock-package defaults
			// by applyDefaults, and validated — StaleTimeout > Heartbeat-
			// Interval). See cross-process.md §Heartbeat Goroutine / §Write
			// Lock and the Options godoc.
			RetryInterval:              opts.LockRetryInterval,
			HeartbeatInterval:          opts.HeartbeatInterval,
			StaleTimeout:               opts.StaleTimeout,
			CrossNamespaceStaleTimeout: opts.CrossNamespaceStaleTimeout,
			// A read-only handle never takes LOCK_EX: the flock-grant
			// goroutine is skipped, but the heartbeat goroutine and the
			// reader-slot path still run so read transactions pin their
			// snapshots against a concurrent cross-process writer.
			ReadOnly: opts.ReadOnly,
		})
	case opts.ReadOnly:
		// Read-only fallback (mmap-strategy.md §Read-Only): the lock
		// file can't be opened read-write — read-only media or
		// read-only filesystem permissions. Proceed lock-free; reads
		// come from the data mmap and don't need the lock file. The
		// trade-off (a concurrent writer on shared storage could
		// reclaim under an in-flight reader) is documented on
		// Options.ReadOnly; a read-only medium normally precludes any
		// writer. lockFile/coord stay nil; BeginRead skips slot
		// acquisition and the teardown paths nil-guard both.
		logger.Warn(
			"gmdb: read-only open could not acquire the lock file; "+
				"proceeding lock-free (no cross-process reader-slot protection)",
			"path", path,
			"detail", mapLockErr(err).Error(),
		)
	default:
		_ = opened.Pager.Close()
		_ = file.Close()
		_ = root.Close()
		return nil, mapLockErr(err)
	}

	// Recovery-commit gate + anchoring (durability.md §Recovery step 5,
	// §Anchoring). Under a briefly-held write grant: reap stale reader
	// slots (OldestReaderTxnID's side effect, LOCK_EX held), then
	// evaluate author liveness via the persisted last-writer record —
	// only the last writer's process can own unfsynced live commits,
	// and it may be idle, so grant/reader occupancy alone cannot gate.
	// No live author + no live readers ⇒ crash residue: publish the
	// durable projection as a recovery commit (rollback happened) or
	// anchor the already-self-durable meta (its assertion may have been
	// read from a surviving page cache). Any liveness ⇒ live join: the
	// selected live tree is a running database's state; adoption is
	// transient and the first grant re-sync converges.
	// pager.Open returned the writer UNATTACHED (which projection to
	// attach is this gate's decision, taken against a grant-current
	// re-read — the pre-grant snapshot can be stale by any number of
	// peer commits that landed while AcquireWriter blocked).
	openRecovered := false
	openTakeoverSeq := uint32(0)
	if !opts.ReadOnly && coord != nil {
		teardown := func() {
			coord.Close()
			_ = lockFile.Close()
			_ = opened.Pager.Close()
			_ = file.Close()
			_ = root.Close()
		}
		grant, gerr := coord.AcquireWriter(ctx)
		if gerr != nil {
			teardown()
			return nil, mapLockErr(gerr)
		}
		_ = coord.OldestReaderTxnID() // reap stale slots in place
		gatedArmRan := false
		if !coord.PrevLastWriterLive() && coord.CountActiveReaders() == 0 {
			gatedArmRan = true
			rm, ridx, recovered, rerr := opened.Pager.RecoverToDurable(file)
			if rerr != nil {
				grant.Release()
				teardown()
				return nil, mapPagerErr(rerr)
			}
			if recovered {
				// Fires on the durable-rollback case AND on the
				// divergent-carrier republication (durability.md
				// §Recovery step 5), where durable == live and
				// nothing is lost — the message covers both.
				logger.Warn("gmdb: recovery commit published (rolled back to the durable epoch, or republished a divergent self-durable carrier)",
					"path", path,
					"durableEpoch", rm.Durable.AnchoredTxnID,
					"recoveryTxnID", rm.TxnID)
			}
			opened.Meta, opened.ActiveMetaIdx = rm, ridx
			openRecovered = recovered
		} else {
			// Live join (or live readers present): attach the latest
			// valid meta's LIVE projection — a running database's
			// current state must not be rolled back.
			lm, lidx, lerr := opened.Pager.AttachLatest(file)
			if lerr != nil {
				grant.Release()
				teardown()
				return nil, mapPagerErr(lerr)
			}
			// This arm's lineage may include a poisoned handle whose
			// failed data fdatasync dropped pwrites from writeback (a
			// same-process re-Open after the documented
			// DurabilityUnknown recovery classifies live and lands
			// here), and the LIVE projection may reference that
			// lineage's dropped DATA pages. The covered-through gate
			// pays the extent rewrite only when the takeover sequence
			// records an uncovered lineage (durability.md §Anchoring).
			if rerr := coverDroppedWritebackLineage(coord, opened.Pager, lm); rerr != nil {
				grant.Release()
				teardown()
				return nil, mapPagerErr(rerr)
			}
			opened.Meta, opened.ActiveMetaIdx = lm, lidx
		}
		// Both attach arms rebuilt bitmap + RPL from the current
		// on-disk image; cache the takeover sequence under the same
		// grant (this acquisition's own dead-prev bump, if any, is
		// already included — correct: our state postdates it).
		openTakeoverSeq = coord.TakeoverSeq()
		if gatedArmRan {
			// The gated arm's completed barrier — the recovery
			// commit's fdatasync, or the self-durable anchor
			// rewrite's — neutralized every recorded lineage: the
			// adopted durable projection references only
			// barrier-covered pages, its bitmap region was redirtied
			// and synced, and the lineage's orphaned data pages are
			// rewritten before any reallocation can reference them.
			// Close the covered-through gate here, where the coord
			// handle lives (durability.md §Anchoring).
			coord.SetRedirtyCoveredSeq(openTakeoverSeq)
		}
		grant.Release()
	}

	db := &DB{
		file:      file,
		root:      root,
		path:      path,
		pool:      pool,
		opts:      opts,
		logger:    logger,
		lockFile:  lockFile,
		coord:     coord,
		pgr:       opened.Pager,
		closeGate: closegate.New(),
		readOnly:  opts.ReadOnly,
	}
	db.adoptOpened(opened)
	db.takeoverSeqSeen = openTakeoverSeq
	if lockFile != nil {
		// Seed the notification region's global version word from the
		// adopted meta (CAS-max: never lowers a peer-advanced value) so
		// versions keep ascending across a lock-file recreation on an
		// uncompacted database (cross-process.md §Lock File Layout,
		// notification region).
		lockFile.SeedNotifyVersion(opened.Meta.TxnID)
	}
	if coord != nil {
		db.dataGeneration.Store(coord.DataGeneration())
		// Inode verification AFTER the generation cache read: if a
		// peer's Compact replaced the path's inode since our fd was
		// opened, the fd and the path disagree — tear down and let
		// Open retry against the current inode. (Ordering argument:
		// the rename happens-before the bump, so a same-inode stat
		// here proves any not-yet-observed bump belongs to a LATER
		// compact, which the per-op checks catch.)
		fdInfo, ferr := file.Stat()
		pathInfo, perr := root.Stat(base)
		if ferr != nil || perr != nil {
			// Genuine stat failure (out-of-band deletion, fd trouble)
			// — not the replaced-inode retry case; surface it as its
			// own inspectable error, no retry.
			_ = db.Close()
			return nil, fmt.Errorf("gmdb: open %q: inode verification: %w", path, errors.Join(ferr, perr))
		}
		if !os.SameFile(fdInfo, pathInfo) {
			_ = db.Close()
			return nil, errStaleInodeAtOpen
		}
		// Benign false positive: an fd that landed on the NEW inode
		// between a peer's rename and bump caches the pre-bump value,
		// passes SameFile, and this handle's first operation then
		// spuriously poisons a correctly-mapped handle — safe-
		// conservative, one Close + re-Open converges.
	}

	// DB-level leak-detection cleanup. The cleanup info captures
	// resources by pointer (not via *DB) so a leaked-then-collected
	// DB doesn't nil-deref when the cleanup fires. The shared
	// The shared closeGate is the gate: if Close() ran, the cleanup
	// is Stop()'d AND a defensive Swap(true) returns true → cleanup
	// exits silently.
	db.cleanup = runtime.AddCleanup(db, dbCleanupFn, dbCleanupInfo{
		gate:      db.closeGate,
		coord:     coord,
		lockFile:  lockFile,
		pgr:       opened.Pager,
		file:      file,
		root:      root,
		logger:    db.logger,
		originPCs: captureOriginPCs(),
	})

	// Start the background maintenance goroutine (background-maintenance.md)
	// unless disabled. A recovering Open (the recovery commit ran — state
	// was rolled back to the durable epoch) schedules the first pass
	// immediately rather than waiting a full interval, to reclaim any
	// crash-leaked pages promptly.
	if !opts.Maintenance.Disable && !opts.ReadOnly {
		db.maint.ctx, db.maint.cancel = context.WithCancel(context.Background())
		db.maint.done = make(chan struct{})
		db.maint.started = true
		// Weak handoff — see maintenanceLoop's doc: the daemon must
		// not pin an abandoned handle reachable.
		go maintenanceLoop(weak.Make(db), db.maint.ctx, db.maint.done, opts.Maintenance.Interval, openRecovered)
	}
	return db, nil
}

// dbCleanupHookForTest fires at the tail of dbCleanupFn (either
// branch) — the leak test's deterministic wait, mirroring
// readTxCleanupHookForTest. Non-blocking callback required.
var dbCleanupHookForTest atomic.Pointer[func()]

// dbCleanupInfo bundles the resources a leaked-DB cleanup needs to
// tear down. Captures the shared *closegate.Gate by pointer (leak-
// detection.md clause-explicit invariant — gate must survive a
// collected-first *DB); resource pointers are independent of the
// *DB so a collected *DB doesn't dangle them.
type dbCleanupInfo struct {
	gate      *closegate.Gate
	coord     *lock.Coord
	lockFile  *lock.File
	pgr       *pager.Pager
	file      *os.File
	root      *os.Root
	logger    *slog.Logger
	originPCs []uintptr
}

// dbCleanupFn is the runtime.AddCleanup callback for a leaked *DB.
// Swap(true) is the same gate as Close uses; whoever wins releases
// the resources. If Close ran first, its Stop() also de-registered
// the cleanup, so the callback usually doesn't fire at all — this
// path covers the race where the cleanup was queued before Stop
// could cancel it.
//
// Unlike the Tx cleanup, the DB cleanup CAN block on coord.Close
// (which waits for goroutine done-channels). Per leak-detection.md
// the non-blocking constraint is scoped to the Tx cleanup
// (high-frequency); only one DB cleanup ever fires per *DB and the
// drain is bounded by Options.LockRetryInterval +
// Options.HeartbeatInterval.
func dbCleanupFn(info dbCleanupInfo) {
	defer func() {
		if hook := dbCleanupHookForTest.Load(); hook != nil {
			(*hook)()
		}
	}()
	if info.gate.SwapClosed(true) {
		// Close() ran first; nothing to tear down.
		return
	}
	info.logger.Warn(
		"gmdb: DB handle leaked without Close",
		"origin", formatStack(info.originPCs),
	)
	// Drain in-flight Tx cleanup windows before teardown — the same
	// step Close performs (leak-detection.md §Close Ordering). The
	// SwapClosed above already release-stored closed=true; the drain
	// waits out any cleanup that passed the gate BEFORE that store
	// and may still be touching the lock-file mmap (a leaked-ReadTx
	// cleanup's slot release). Today's runtime executes cleanups
	// sequentially on one goroutine, which made this unreachable —
	// but the API does not guarantee it, and a concurrent-cleanups
	// runtime would otherwise race this teardown's unmap (SIGSEGV).
	// BeginClose is idempotent over the prior swap and, on the
	// sequential runtime, returns immediately (no cleanup can be
	// mid-window while we run).
	info.gate.BeginClose()
	if info.coord != nil {
		_ = info.coord.Close()
	}
	if info.lockFile != nil {
		_ = info.lockFile.Close()
	}
	if info.pgr != nil {
		_ = info.pgr.Close()
	}
	if info.file != nil {
		_ = info.file.Close()
	}
	if info.root != nil {
		_ = info.root.Close()
	}
}

// Close releases all resources held by the DB handle. After Close, the
// handle is unusable; subsequent Begin returns ErrClosed. Idempotent.
//
// Spec contract (leak-detection.md §Close Ordering clause-explicit
// invariant + cross-process.md Close-releases invariant):
//
//  1. Win the close CAS (release-store on the shared *atomic.Bool).
//     Visible to any subsequent Tx cleanup callback regardless of
//     runtime.AddCleanup ordering between the DB and its Txs — a
//     WRITE-Tx cleanup observes it and skips the flock signal; a
//     READ-Tx cleanup releases its slot regardless, through its own
//     lifetime reference on the lock-file mapping (step 8). A second
//     Close returns immediately.
//  2. Stop the batch coordinator (context cancel; its in-flight
//     write transaction unwinds and releases the grant first).
//  3. Stop the maintenance goroutine and wait for exit.
//  4. Drain in-flight Tx cleanup windows (closeGate.BeginClose spins
//     on the txInflight counter).
//  5. The shutdown checkpoint (durability.md §Clean shutdown): a
//     writable, non-poisoned handle checkpoints under the write
//     grant so a clean close never loses acknowledged commits
//     regardless of SyncMode. This step can BLOCK on a cross-process
//     grant holder (bounded by the peer's commit window in healthy
//     operation, unbounded if the peer is wedged — the same waiting
//     semantics as Begin); an IN-process live write transaction
//     instead skips the step with a warning (see shutdownCheckpoint).
//     A checkpoint failure here becomes Close's return error;
//     teardown proceeds regardless.
//  6. Capture and nil the resource pointers under db.mu.
//  7. Drain the heartbeat + flock goroutines via Coord.Close (which
//     blocks on done-channels; the flock goroutine clears writer-
//     header fields, unlocks a held writer, and fails pending
//     acquisitions).
//  8. Drop the handle's reference on the lock-file mapping (munmap
//     and lock-file fd close happen at the LAST drop — an open
//     ReadTx's reference keeps them alive), close the pager
//     (data-file munmap), the data-file fd, and the *os.Root.
//
// Steps 1 → 4 ordering: the CAS is the public release-store; the
// drain completes before any teardown. Step 5 sits after the stops
// (they release grants it needs) and before the pointer capture (it
// needs the live pager). Steps 7 → 8 ordering: a final heartbeat
// tick must never land on unmapped memory, so the goroutines drain
// before the handle's mapping reference drops.
//
// Returns the shutdown checkpoint's error, if any (nil otherwise;
// the CAS loser of a concurrent double-Close always returns nil).
//
// Not safe to call concurrently with active write or batch
// transactions in the same process; per leak-detection.md
// §Close Ordering, callers must commit or rollback all
// transactions before Close.
//
// Close-vs-dbCleanupFn race: the shared closeGate guards
// against double-drain (cleanup's Swap(true) returns true if Close
// won the CAS first; Close returns nil if the cleanup won the
// Swap first). The race is unreachable under normal use — for
// Close() to be invoked, *DB must be reachable from the calling
// goroutine, but for dbCleanupFn to fire, GC must have determined
// *DB unreachable. The gate exists as defense-in-depth against
// runtime.AddCleanup ordering pathologies. See leak-detection.md
// §Database Handle Leak Detection for the full discussion.
func (db *DB) Close() error {
	// Step 1 — release-store closed=true via gate.CAS. Idempotency:
	// a second Close returns immediately because only one caller
	// wins the CAS.
	if !db.closeGate.CompareAndSwapClosed(false, true) {
		return nil
	}
	// Step 2 — stop the batch coordinator (transactions.md §Write
	// Batching coordinator lifecycle). Cancel its context (refusing new
	// batches and unblocking any pending write-lock wait) and wait for it
	// to exit. Done before the Coord / pager teardown below so the
	// coordinator's in-flight write transaction unwinds and releases its
	// grant first, and no coordinator goroutine outlives the unmap.
	db.stopBatchCoordinator()
	// Step 3 — stop the maintenance goroutine and wait for it to exit.
	// Done before the Coord / pager teardown so an in-flight pass's tx
	// unwinds and the goroutine never touches a torn-down mmap (leak-detection.md §Close() Ordering).
	db.stopMaintenance()
	// Step 4 — drain in-flight Tx cleanups. Cleanups that observed
	// closed=false BEFORE our store may still be mid-work touching
	// the lock-file mmap (the read-tx slot-release path); we MUST
	// wait for them to complete before unmapping. The gate's
	// txInflight counter (incremented at the top of every cleanup,
	// decremented at the bottom regardless of skip/work) lets us
	// spin until quiescence. See internal/closegate for the interleaving
	// analysis. Cleanups that fire AFTER this step observe
	// closed=true and skip the resource-touching work.
	//
	// We've already won the CAS — perform the drain via the same
	// gate. BeginClose stores closed=true (idempotent — already
	// true from our CAS above) and drains.
	db.closeGate.BeginClose()

	// Step 5 — the shutdown checkpoint (durability.md §Clean
	// shutdown). After the CAS + coordinator/maintenance stops +
	// cleanup drain: no new Begin can start (gate closed) and any
	// in-flight write tx completed before our grant acquisition, so
	// every acknowledged commit is covered by the bump. Before the
	// pointer capture: the pager and file must still be alive.
	closeErr := db.shutdownCheckpoint()

	// Step 6 — capture resource pointers under db.mu so a concurrent
	// Begin (which snapshots db.coord under db.mu) sees a consistent
	// view — either pre-close (non-nil) or post-nil (nil). The
	// captured locals are then used outside db.mu for the actual
	// drain, which can take milliseconds.
	db.mu.Lock()
	coord := db.coord
	lockFile := db.lockFile
	pgr := db.pgr
	file := db.file
	root := db.root
	db.coord = nil
	db.lockFile = nil
	db.pgr = nil
	db.file = nil
	db.root = nil
	db.mu.Unlock()

	// Step 7 — drain goroutines. Coord.Close blocks until both the
	// flock goroutine and the heartbeat goroutine have exited; with
	// a writer held at Close time, the stopCh path clears the
	// writer-header fields and issues flock(LOCK_UN) before exit.
	if coord != nil {
		_ = coord.Close()
	}
	// Step 8 — drop the handle's lifetime reference on the lock-file
	// mapping (leak-detection.md §Close() Ordering step 8). The
	// munmap happens at the LAST drop: any still-open ReadTx holds
	// its own reference from BeginRead, keeping its reader slot
	// mapped — bound-pinning and releasable — until its own close.
	// Goroutine safety: Coord.Close drained heartbeat + flock
	// goroutines; closeGate.BeginClose drained BeginRead windows.
	if lockFile != nil {
		_ = lockFile.Close()
	}
	// Step 8 (cont.) — release pager (munmaps data file).
	if pgr != nil {
		_ = pgr.Close()
	}
	// Step 8 (cont.) — close fds.
	if file != nil {
		_ = file.Close()
	}
	if root != nil {
		_ = root.Close()
	}

	// Cancel the DB-level leak cleanup — we closed cleanly.
	db.cleanup.Stop()
	return closeErr
}

// Begin starts a write transaction. The call blocks until the
// cross-process write lock can be acquired (cross-process.md §Write
// Lock), ctx is cancelled, or the DB is closed.
//
// Returns:
//   - context.Cause(ctx) if ctx fires before grant.
//   - ErrClosed if the DB's coordination goroutines have shut down.
//   - ErrPoisoned if a previous tx's commit poisoned the handle.
//
// Internal structure: two short db.mu critical sections bracket the
// (potentially long-blocking) coord.AcquireWriter call. The first —
// inside acquireWriteGrant — snapshots db.coord (race-safe vs Close,
// which nil's it under db.mu; db.pgr is first read post-grant, where
// resyncOnGrantLocked re-reads it under db.mu anyway because Compact
// can swap it); the second post-acquire critical section resyncs,
// captures prev meta, builds the Tx, and registers leak-detection
// cleanup. db.mu is NOT held across AcquireWriter — doing so would
// let Close deadlock waiting for db.mu while the coord's stopCh is
// closed but the flock goroutine still holds the result-channel send.
//
// Read transactions use the distinct BeginRead (returning *ReadTx) so
// the type system rejects write methods on a read snapshot at compile
// time (api-surface.md §Database and Transaction API).
func (db *DB) Begin(ctx context.Context) (*Tx, error) {
	// acquireWriteGrant runs the shared preamble; read-only rejection
	// comes first there (api-surface.md §Options.ReadOnly — a
	// permanent property, so the cause is unambiguous; this also
	// covers Update, which begins through here).
	grant, coord, err := db.acquireWriteGrant(ctx)
	if err != nil {
		return nil, err
	}

	db.mu.Lock()
	defer db.mu.Unlock()

	pgr, _, err := db.resyncOnGrantLocked()
	if err != nil {
		grant.Release()
		return nil, err
	}

	prevMeta := db.currentMeta
	prevActive := db.activeMetaIdx
	// TxnID seeded at tx-start so the LaggingReader callback's
	// Lag = currentTxnID - reclamationBound is meaningful when
	// AllocPage fires from the user path (Keyspace.Put → btree.Put →
	// pw.AllocPage) — the public Keyspace ops never go through
	// Tx.AllocPage's re-seed.
	newTxnID := prevMeta.TxnID + 1

	// LaggingReader wiring: the wrapper checks
	// coord.OldestReaderTxnID() before invoking the user callback —
	// when no reader is active, OldestReaderTxnID returns
	// lock.NoReaderTxnID and the RPL-blocked condition is a
	// checkpoint-boundary effect (segments retired by prevMeta's
	// commit pinned by `< bound` strict-inequality), not a
	// lagging-reader case. Per lock-ordering.md §Lagging Reader
	// Handling, the callback fires only when "a reader in the reader
	// table is blocking reclamation." Skip the user callback in the
	// no-reader case and return Wait so the pager falls through to
	// file extension.
	var lagging func(pager.LaggingReaderInfo) pager.LaggingReaderAction
	if userCallback := db.opts.LaggingReader; userCallback != nil {
		lagging = func(info pager.LaggingReaderInfo) pager.LaggingReaderAction {
			if coord.OldestReaderTxnID() == lock.NoReaderTxnID {
				return pager.LaggingReaderWait
			}
			publicInfo := LaggingReaderInfo{
				PID:       info.PID,
				TxnID:     info.TxnID,
				Lag:       info.Lag,
				HeldPages: info.HeldPages,
			}
			switch userCallback(publicInfo) {
			case LaggingReaderWait:
				return pager.LaggingReaderWait
			case LaggingReaderAbort:
				return pager.LaggingReaderAbort
			default:
				return pager.LaggingReaderWait
			}
		}
	}

	// We hold flock(LOCK_EX) via the grant, satisfying
	// reclamationBound's precondition (BeginTx seeds the bound
	// synchronously; refreshes fire inside AllocPage, still under the
	// grant); see reclamationBound's doc for why the bound uses
	// the anchored epoch rather than prevMeta.TxnID. The RPLCorrupt
	// callback surfaces quarantined-segment corruption (free-space.md
	// §RPL Reclamation) which would otherwise be invisible until an
	// explicit Check(): log it (db.logger defaults to a discard
	// handler); DBStats carries the count for programmatic detection.
	// Callbacks stored on the long-lived writer pager must not
	// capture *db: the leaked-DB cleanup's info holds the pager
	// strongly (runtime → dbCleanupInfo → pgr → callback), so a db
	// capture would pin an abandoned handle reachable forever and the
	// cleanup could never fire (leak-detection.md §Database Handle
	// Leak Detection). Capture the needed fields instead.
	logger := db.logger
	// Captured under db.mu for the ShrinkGate closure: the gate runs
	// at commit time under the write grant, and Close cannot proceed
	// past its own grant acquisition while this tx holds it, so the
	// capture stays valid for the closure's lifetime.
	lockFileForGate := db.lockFile
	pgr.BeginTx(pager.TxParams{
		HighWaterMark: prevMeta.HighWaterMark,
		MaxSize:       prevMeta.MaxSize,
		GrowStep:      prevMeta.GrowStep,
		MinSize:       prevMeta.MinSize,
		TxnID:         newTxnID,
		ReclamationBound: func() uint64 {
			return reclamationBound(coord, pgr)
		},
		RPLCorrupt: func(segPageID uint64) {
			logger.Warn("gmdb: corrupt RPL segment quarantined during reclamation; "+
				"its pages leak until Check()/Repair reclaims them",
				"segPageID", segPageID)
		},
		LaggingReader: lagging,
		ShrinkGate: func(truncate func() error) error {
			// Shrink defers while any reader is live (file-format.md
			// §File Shrinkage): a reader's file-resident bound is
			// fixed at Begin; truncating under it turns corrupt
			// content-derived page ids into SIGBUS instead of the
			// contracted ErrCorrupted. We hold the write grant, so
			// the reader-table scan's LOCK_EX precondition holds.
			if coord == nil {
				return truncate()
			}
			// Seqlock bracket: odd while the scan→truncate span is
			// open, even when settled. A reader publishing its slot
			// AFTER the scan passed it brackets its own file-size
			// read against this counter and re-reads on overlap —
			// closing the acquisition window during which it would
			// otherwise retain a pre-shrink bound for its lifetime.
			if lockFileForGate == nil {
				return nil // no lock file (raced teardown): skip
			}
			lockFileForGate.BumpShrinkSeq()
			defer lockFileForGate.BumpShrinkSeq()
			if coord.CountActiveReaders() != 0 {
				return nil // defer: reader visible
			}
			if hook := shrinkGateHookForTest.Load(); hook != nil {
				(*hook)()
			}
			return truncate()
		},
	})

	held := &atomic.Bool{}
	held.Store(true)
	tx := &Tx{
		db:           db,
		pgr:          pgr,
		prevMeta:     prevMeta,
		prevActive:   prevActive,
		newTxnID:     newTxnID,
		writable:     true,
		held:         held,
		grant:        grant,
		keyspaceRoot: prevMeta.KeyspaceRoot,
		numKeyspaces: prevMeta.NumKeyspaces,
		startTime:    time.Now(), // TxStats.Duration anchor
	}
	// Wire the leak-detection cleanup per leak-detection.md. The
	// cleanup info captures the shared *closegate.Gate by pointer
	// (clause-explicit invariant — required for cleanup to observe
	// Close without nil-deref'ing through a potentially-collected
	// *DB) plus *Pager, *Grant, the held atomic, and the origin
	// stack. Never references *Tx itself (resurrection-forbidden).
	tx.cleanup = runtime.AddCleanup(tx, txCleanupFn, txCleanupInfo{
		gate:      db.closeGate,
		pgr:       pgr,
		grant:     grant,
		held:      held,
		logger:    db.logger,
		originPCs: captureOriginPCs(),
	})
	return tx, nil
}

// acquireWriteGrant runs the write-grant preamble shared by Begin,
// Checkpoint, and Compact: the fast readOnly/closed/poisoned gates, the
// coord snapshot (race-safe vs Close, which nil's it under db.mu),
// cross-process acquisition, and the post-grant re-checks — poison (a
// concurrent commit's publication failure while we waited; proceeding
// would build on state whose fsync error the kernel already consumed),
// close (Close ran while we waited in AcquireWriter), and the data-file
// generation check (cross-process.md §Data-file generation: a peer's
// Compact replaced the inode this handle still maps — writing through
// it would silently diverge from every other process; the handle
// poisons itself). On success the caller owns the returned grant.
// db.mu is NOT held across AcquireWriter — that would let Close
// deadlock waiting for db.mu while the coord's flock goroutine holds
// the result-channel send.
//
// Post-grant re-check order is poison → close → generation. The order
// is unobservable: each check only picks which error a doomed caller
// gets, and the un-stored poison flag in the
// close-and-generation-both-true race is invisible (every entry point
// fails the closed fast-gate before consulting it).
func (db *DB) acquireWriteGrant(ctx context.Context) (*lock.Grant, *lock.Coord, error) {
	if db.readOnly {
		return nil, nil, ErrDatabaseReadOnly
	}
	if db.closeGate.IsClosed() {
		return nil, nil, ErrClosed
	}
	// Poison check before acquiring the cross-process lock so a
	// poisoned handle does not block legitimate concurrent callers
	// across processes.
	if db.poisoned.Load() {
		return nil, nil, ErrPoisoned
	}
	coord := db.coordSnapshot()
	if coord == nil {
		return nil, nil, ErrClosed
	}
	grant, err := coord.AcquireWriter(ctx)
	if err != nil {
		if errors.Is(err, lock.ErrClosed) {
			return nil, nil, ErrClosed
		}
		return nil, nil, err
	}
	if db.poisoned.Load() {
		grant.Release()
		return nil, nil, ErrPoisoned
	}
	if db.closeGate.IsClosed() {
		grant.Release()
		return nil, nil, ErrClosed
	}
	if gen := coord.DataGeneration(); gen != db.dataGeneration.Load() {
		db.poisoned.Store(true)
		db.logger.Error("gmdb: data file replaced by a peer Compact; handle poisoned — Close and re-Open",
			"cachedGeneration", db.dataGeneration.Load(), "currentGeneration", gen)
		grant.Release()
		return nil, nil, ErrPoisoned
	}
	return grant, coord, nil
}

// resyncOnGrantLocked re-reads pgr/file under db.mu (a Compact may have
// swapped them while the caller waited for the grant — the pre-grant
// captures may name the closed, munmap'd old pager) and re-syncs the
// writer's in-memory state (meta, bitmap, RPL, fileSize) from disk: a
// peer process holding the grant before us may have committed, leaving
// our view stale. Without this a serialized cross-process writer builds
// on a stale root (lost update), writes its meta over the slot holding
// the peer's newer commit, and allocates from a stale bitmap (page
// aliasing) — cross-process.md §Writer acquisition flow. Cheap when
// the on-disk active TxnID is unchanged (the common single-writer
// path): the bitmap/RPL rebuild is skipped, but the cached meta is
// refreshed unconditionally (a peer's checkpoint bump changes the
// sub-record without changing TxnID). On a Resync error the pager is fully unmodified
// (attachState is atomic), so the handle stays usable — the caller
// releases the grant and surfaces the error; no poison needed.
//
// Caller holds db.mu AND the write grant.
func (db *DB) resyncOnGrantLocked() (*pager.Pager, *os.File, error) {
	// Race-with-Close under the mu the caller holds: surface ErrClosed
	// rather than resync against a torn-down pager.
	if db.closeGate.IsClosed() {
		return nil, nil, ErrClosed
	}
	return db.resyncPagerLocked()
}

// resyncPagerLocked is resyncOnGrantLocked without the close-gate
// check — the shutdown checkpoint (durability.md §Clean shutdown)
// runs after Close wins the close CAS but before teardown, when the
// gate is closed yet the pager is still fully alive. Every other
// caller routes through resyncOnGrantLocked.
func (db *DB) resyncPagerLocked() (*pager.Pager, *os.File, error) {
	pgr := db.pgr
	file := db.file
	if pgr == nil || file == nil {
		return nil, nil, ErrClosed
	}
	// Grant-handoff tear detection (free-space.md §Grant-handoff
	// tear detection): a writer that died mid-reclamation left torn
	// bitmap writes with NO TxnID advance, so the equality skip
	// would keep every surviving handle's chain predating the tear,
	// and reclamation behind the tear's reclaimed boundary
	// double-frees. The takeover sequence is level-triggered: bumped
	// under LOCK_EX at either tear source (an acquisition observing
	// the died-holding-grant writer header, or a publication-phase
	// commit failure's own poison-site bump), and compared here
	// against the value cached at THIS handle's last rebuild — the
	// header alone is edge-triggered and consumed by any
	// intermediate acquisition (including another handle of our own
	// process).
	force := false
	var seq uint32
	if coord := db.coord; coord != nil {
		if seq = coord.TakeoverSeq(); seq != db.takeoverSeqSeen {
			force = true
		}
	}
	m, active, _, err := pgr.Resync(file, db.currentMeta.TxnID, force)
	if err != nil {
		return nil, nil, fmt.Errorf("gmdb: re-sync writer state on grant: %w", mapPagerErr(err))
	}
	if force {
		// The bump that forced this rebuild may record a poison/death
		// lineage whose failed barrier dropped pwrites from writeback;
		// the covered-through gate redirties the attached extent and
		// barriers over it exactly once across all handles
		// (durability.md §Anchoring). Error posture mirrors Resync:
		// surface without poisoning, the gate stays open, a retry
		// re-runs it.
		if cerr := coverDroppedWritebackLineage(db.coord, pgr, m); cerr != nil {
			return nil, nil, fmt.Errorf("gmdb: re-sync writer state on grant: %w", mapPagerErr(cerr))
		}
		db.takeoverSeqSeen = seq
	}
	// Refresh the cached meta UNCONDITIONALLY, not only on a TxnID
	// change: a peer's CHECKPOINT BUMP rewrites the active slot's
	// durable sub-record without changing TxnID, so a TxnID-equality
	// gate would leave db.currentMeta stale — the next commit's
	// buildNewMeta then carries a RETREATED durable epoch into its
	// meta (a crash after it discards the peer's checkpointed epochs),
	// and Checkpoint would evaluate SelfDurable() on the pre-bump
	// struct and pwrite CHANGED bytes over the self-durable slot that
	// is the sole durable carrier of its own assertion. Resync's own
	// TxnID-equality check still gates the expensive bitmap/RPL
	// rebuild — only the cached-meta refresh is unconditional.
	db.setMetaState(m, active)
	return pgr, file, nil
}

// shutdownCheckpoint is Close's clean-shutdown step (durability.md
// §Clean shutdown): a writable, non-poisoned handle checkpoints under
// the grant before teardown, so a clean close never loses
// acknowledged commits regardless of SyncMode. A poisoned handle
// SKIPS it — running it would be exactly the retried-fsync trap of
// §Checkpoint failure semantics. A generation mismatch (peer Compact
// replaced the inode) also skips: our mapped file is unlinked and
// invisible; there is nothing on it worth making durable. Failure is
// returned as Close's error; teardown continues regardless (poison is
// moot on a closing handle).
func (db *DB) shutdownCheckpoint() error {
	if db.readOnly || db.poisoned.Load() {
		return nil
	}
	coord := db.coordSnapshot()
	if coord == nil {
		return nil
	}
	// A live IN-PROCESS write tx holds the grant across this Close
	// (the app closed mid-transaction) and cannot release until after
	// Close returns — waiting would deadlock. Skip: an app closing
	// mid-tx is not the clean close the §Clean shutdown guarantee
	// addresses, and the leaked tx's own cleanup path warns. A
	// cross-process holder is waited out below (bounded by the peer's
	// commit window).
	//
	// Advisory-flag residual: a Begin racing this Close can hold the
	// grant for a microsecond window while DOOMED to bounce at its
	// own post-grant close check — WriterHeld then reads true and
	// this Close skips a checkpoint it could have run. Conservative
	// direction only (the skip equals the pre-shutdown-checkpoint
	// Close semantics for that call), reachable only by racing Begin
	// against Close, and irreducible without holding db.mu across
	// grant acquisition (a documented deadlock).
	if coord.WriterHeld() {
		db.logger.Warn("gmdb: Close while an in-process grant holder is live; shutdown checkpoint skipped")
		return nil
	}
	grant, err := coord.AcquireWriter(context.Background())
	if err != nil {
		if errors.Is(err, lock.ErrClosed) {
			return nil
		}
		return fmt.Errorf("gmdb: shutdown checkpoint: %w", err)
	}
	defer grant.Release()
	if db.poisoned.Load() {
		return nil
	}
	if coord.DataGeneration() != db.dataGeneration.Load() {
		return nil
	}
	if err := db.checkpointUnderGrant(); err != nil {
		return fmt.Errorf("gmdb: shutdown checkpoint: %w", err)
	}
	return nil
}

// setMetaState installs the meta baseline pair — currentMeta and
// activeMetaIdx name the committed snapshot — so every update site
// funnels through this one assignment. The RPL reclamation bound no
// longer rides here: it derives from the pager's anchored epoch
// (durability.md §Anchoring), which the pager maintains at its fsync
// sites. Caller holds db.mu (or has exclusive access: Open's
// construction, Compact's swap under grant+db.mu).
func (db *DB) setMetaState(m pager.Meta, active int) {
	db.currentMeta = m
	db.activeMetaIdx = active
}

// adoptOpened installs the meta baseline from a fresh pager Open —
// db.Open's construction and Compact's post-swap reopen. The meta is
// the LIVE projection; the recovery-commit gate (db.Open) has already
// republished the durable projection when that was warranted.
func (db *DB) adoptOpened(opened *pager.OpenedDB) {
	db.setMetaState(opened.Meta, opened.ActiveMetaIdx)
}

// coordSnapshot returns db.coord captured under db.mu — the race
// protection against a concurrent Close, which nils the pointer under
// the same mutex (sites that need more fields capture their own set
// inline under one db.mu section). nil means closing/closed.
func (db *DB) coordSnapshot() *lock.Coord {
	db.mu.Lock()
	defer db.mu.Unlock()
	return db.coord
}

// reclamationBound derives the RPL reclamation bound
// min(oldestActiveReaderTxnID, anchoredEpoch) per free-space.md §RPL
// Reclamation. The caller MUST hold the cross-process write grant —
// coord.OldestReaderTxnID's flock(LOCK_EX) precondition
// (cross-process.md §Writer's Page Reclamation). With no live readers
// the scan returns lock.NoReaderTxnID, reducing the min to the
// anchored epoch. The bound MUST use the ANCHORED epoch — the newest
// DurableTxnID assertion a completed fdatasync has covered
// (durability.md §Anchoring), maintained by the pager at its fsync
// sites — never a raw meta TxnID or an unanchored assertion:
// reclaiming past what the disk records frees data pages the tree a
// crash recovers to still references — deterministic corruption.
func reclamationBound(coord *lock.Coord, pgr *pager.Pager) uint64 {
	return min(coord.OldestReaderTxnID(), pgr.AnchoredEpoch())
}

// readPersistedPageSize discovers the file's PageSize via pager.DiscoverPageSize,
// retrying on the EEXIST-retry fallback (raceWindow=true) so the loser
// of an O_CREATE|O_EXCL race waits for the winner to finish Init. The
// retry catches the partial-initialisation window only; a permanent
// corruption falls through to the wrapped ErrCorrupted from
// DiscoverPageSize after the final attempt. The lock file
// resolves the race window structurally; the retry remains here as a
// defensive measure.
func readPersistedPageSize(ctx context.Context, file *os.File, raceWindow bool) (uint32, error) {
	const (
		maxAttempts = 50
		backoff     = 2 * time.Millisecond
	)
	attempts := 1
	if raceWindow {
		attempts = maxAttempts
	}
	var lastErr error
	for i := range attempts {
		ps, err := pager.DiscoverPageSize(file)
		if err == nil {
			return ps, nil
		}
		lastErr = err
		// A version mismatch is deterministic, not the partial-init race
		// the retry loop exists for — fail fast.
		if errors.Is(err, pager.ErrVersionMismatch) {
			break
		}
		if i+1 < attempts {
			// ctx-aware backoff: a cancelled / timed-out context aborts
			// the partial-init retry promptly instead of sleeping it out.
			select {
			case <-ctx.Done():
				return 0, context.Cause(ctx)
			case <-time.After(backoff):
			}
		}
	}
	return 0, mapPagerErr(lastErr)
}

// mapLockErr translates lock-package sentinels to the root package's
// public sentinels.
//
//   - lock.ErrCorrupted → ErrCorrupted (wrapped — preserves descriptive
//     suffix like "magic mismatch" / "size mismatch").
//   - lock.ErrInvalidMaxReaders → ErrInvalidOptions (fires when the
//     caller-supplied Options.MaxReaders is outside [1, 65536]).
//     Options.validate also pre-checks this so the error surfaces
//     before the data-file is opened; the lock-package check remains
//     as a defense-in-depth boundary.
//   - lock.ErrInvalidBase → ErrInvalidOptions: defensive — db.Open
//     derives Base from filepath.Base, which excludes the rejected
//     characters (`/`, `\x00`), so this branch is in practice
//     unreachable from the public API. Kept to fail loud rather than
//     silent if a future caller bypasses BaseFor.
//
// Other errors pass through wrapped under a "gmdb: lock" prefix.
//
// lock.ErrClosed has a dedicated branch for symmetry with the inline
// check in Begin: today only Coord.AcquireWriter returns it (handled
// inline at db.go's Begin), but a future caller routing Coord errors
// through mapLockErr would otherwise hit the `default` branch and
// surface a wrapped lock-package string instead of the user-facing
// ErrClosed sentinel.
func mapLockErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, lock.ErrCorrupted):
		return fmt.Errorf("%w: %w", ErrCorrupted, err)
	case errors.Is(err, lock.ErrInvalidBase), errors.Is(err, lock.ErrInvalidMaxReaders):
		return fmt.Errorf("%w: %w", ErrInvalidOptions, err)
	case errors.Is(err, lock.ErrClosed):
		return ErrClosed
	default:
		return fmt.Errorf("gmdb: lock: %w", err)
	}
}

// Update is a convenience wrapper: begin a write tx, call fn, commit on
// success or rollback on error. If fn returns an error and the rollback
// also fails, both errors are joined via errors.Join so callers can
// inspect either via errors.Is.
func (db *DB) Update(ctx context.Context, fn func(tx *Tx) error) error {
	tx, err := db.Begin(ctx)
	if err != nil {
		return err
	}
	// Panic safety: guarantee the tx is closed (cross-process write grant
	// + pager tx state released) even if fn panics. A recovered panic
	// higher up the stack — idiomatic in Go services — must not leak the
	// grant: that blocks every other writer in this AND any other process
	// on AcquireWriter until GC finalizes the *Tx. `done` is set on the
	// normal commit/rollback paths, so this deferred rollback fires only on
	// a panic unwinding through fn (api-surface.md, Update/View panic safety).
	done := false
	defer func() {
		if !done {
			_ = tx.Rollback()
		}
	}()
	if fnErr := fn(tx); fnErr != nil {
		done = true
		if rbErr := tx.Rollback(); rbErr != nil {
			return errors.Join(fnErr, rbErr)
		}
		return fnErr
	}
	// done stays false through Commit: a Commit that fails WITHOUT
	// closing the tx (ErrChildActive — fn left a child unresolved)
	// must not leak the write grant, so the deferred rollback (which
	// cascades) fires. A publication-phase Commit failure closes the
	// tx itself; the deferred Rollback is then a harmless ErrTxClosed.
	if err := tx.Commit(); err != nil {
		return err
	}
	done = true
	return nil
}

// coverDroppedWritebackLineage runs the dropped-writeback recovery
// rewrite behind the covered-through gate (durability.md §Anchoring).
// Under the held write grant — where the takeover sequence is stable —
// a sequence past the covered-through mark records a poison/death
// lineage nothing has covered: the whole attached extent is redirtied,
// a barrier completes over it, and the gate closes at the sequence
// read. A healthy lineage pays a single header compare. On any error
// the gate stays open (conservative: the next attach retries).
func coverDroppedWritebackLineage(coord *lock.Coord, pgr *pager.Pager, m pager.Meta) error {
	if coord == nil {
		return nil
	}
	seq := coord.TakeoverSeq()
	if coord.RedirtyCoveredSeq() == seq {
		return nil
	}
	if err := pgr.RedirtyAttachedExtent(m); err != nil {
		return err
	}
	if err := pgr.SyncData(); err != nil {
		return err
	}
	coord.SetRedirtyCoveredSeq(seq)
	return nil
}

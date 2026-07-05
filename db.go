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

	"github.com/thegrumpylion/gmdb/internal/lock"
	"github.com/thegrumpylion/gmdb/internal/page"
	"github.com/thegrumpylion/gmdb/internal/pager"
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
	currentMeta   page.Meta
	activeMetaIdx int
	// lastCheckpointTxnID is the TxnID of the most recent checkpoint meta —
	// the meta recovery would select (durability.md §Recovery). It bounds RPL
	// reclamation per free-space.md §RPL Reclamation: min(oldestReaderTxnID,
	// lastCheckpointTxnID). Reclaiming past the last checkpoint would free
	// pages a still-recoverable meta's tree references. Updated to the new
	// TxnID on a checkpoint commit (SyncDurable/SyncDataOnly) and Checkpoint();
	// unchanged on SyncLazy commits. Guarded by db.mu.
	lastCheckpointTxnID uint64
	pgr                 *pager.Pager

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

	// closeGate is a heap-allocated coordination struct shared by
	// pointer with every txCleanupInfo, every readTxCleanupInfo, and
	// the dbCleanupInfo. Composes the (closed bool, txInflight
	// counter) pair that leak-detection.md
	// §Cleanup Behavior + §Close Ordering requires — see closegate.go
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
	closeGate *closeGate

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
			PageChecksum:    opts.PageChecksum,
			MinSize:         opts.MinSize,
			MaxSize:         opts.MaxSize,
			GrowStep:        opts.GrowStep,
			ShrinkThreshold: opts.ShrinkThreshold,
			UUID:            opts.UUID,
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
		if err := syncDir(root); err != nil {
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
	pop := pager.OpenParams{
		Pool:             pool,
		MaxTxBufferBytes: opts.MaxTxBufferBytes,
	}
	var opened *pager.OpenedDB
	if opts.ReadOnly {
		// Read-only handle: build a reader pager (no writer slab / no
		// in-memory bitmap / no RPL chain) — it owns the data mmap and
		// backs handle-level raw reads (Check). Each read tx still spins
		// up its own per-snapshot reader pager (BeginRead).
		opened, err = pager.OpenReadOnly(file, pop)
	} else {
		opened, err = pager.Open(file, pop)
	}
	if err != nil {
		_ = file.Close()
		_ = root.Close()
		return nil, mapPagerErr(err)
	}

	// durability.md §Recovery step 3: recovery accepted a
	// non-checkpoint meta because no checkpoint-flagged meta exists
	// (SyncLazy-only DB never Checkpoint()'d). Warn — data
	// integrity depends on whether the OS flushed pages in the
	// right order, which is not guaranteed. Emitted via the
	// Options.Logger (or discard) once it has been resolved.
	openWarnNoCheckpoint := opened.NoCheckpoint

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
			RetryInterval:     opts.LockRetryInterval,
			HeartbeatInterval: opts.HeartbeatInterval,
			StaleTimeout:      opts.StaleTimeout,
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

	db := &DB{
		file:          file,
		root:          root,
		path:          path,
		pool:          pool,
		opts:          opts,
		logger:        logger,
		lockFile:      lockFile,
		coord:         coord,
		currentMeta:   opened.Meta,
		activeMetaIdx: opened.ActiveMetaIdx,
		// Recovery selected the highest-TxnID checkpoint meta, so its TxnID is
		// the last checkpoint. If no checkpoint meta was found (NoCheckpoint),
		// there is no recoverable checkpoint to bound against, so reclamation
		// is pinned off (bound 0) until the next checkpoint re-establishes it.
		lastCheckpointTxnID: opened.Meta.TxnID,
		pgr:                 opened.Pager,
		closeGate:           newCloseGate(),
		readOnly:            opts.ReadOnly,
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

	if openWarnNoCheckpoint {
		db.lastCheckpointTxnID = 0
		db.logger.Warn(
			"gmdb: Open accepted non-checkpoint meta",
			"path", path,
			"txn_id", opened.Meta.TxnID,
			"detail", "no checkpoint-flagged meta found; data integrity depends on OS flush ordering",
		)
	}
	// DB-level leak-detection cleanup. The cleanup info captures
	// resources by pointer (not via *DB) so a leaked-then-collected
	// DB doesn't nil-deref when the cleanup fires. The shared
	// db.closed atomic is the gate: if Close() ran, the cleanup
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
	// unless disabled. A recovery that accepted a non-checkpoint meta
	// (opened.NoCheckpoint — an unclean prior shutdown) schedules the first
	// pass immediately rather than waiting a full interval, to reclaim any
	// crash-leaked pages promptly.
	if !opts.Maintenance.Disable && !opts.ReadOnly {
		db.maint.ctx, db.maint.cancel = context.WithCancel(context.Background())
		db.maint.done = make(chan struct{})
		db.maint.started = true
		go db.maintenanceLoop(db.maint.ctx, opened.NoCheckpoint)
	}
	return db, nil
}

// dbCleanupInfo bundles the resources a leaked-DB cleanup needs to
// tear down. Captures the shared *closeGate by pointer (leak-
// detection.md clause-explicit invariant — gate must survive a
// collected-first *DB); resource pointers are independent of the
// *DB so a collected *DB doesn't dangle them.
type dbCleanupInfo struct {
	gate      *closeGate
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
	if info.gate.SwapClosed(true) {
		// Close() ran first; nothing to tear down.
		return
	}
	info.logger.Warn(
		"gmdb: DB handle leaked without Close",
		"origin", formatStack(info.originPCs),
	)
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
//     runtime.AddCleanup ordering between the DB and its Txs — they
//     observe the close state and exit without touching torn-down
//     resources. A second Close returns immediately.
//  2. Stop the batch coordinator (context cancel; its in-flight
//     write transaction unwinds and releases the grant first).
//  3. Stop the maintenance goroutine and wait for exit.
//  4. Drain in-flight Tx cleanup windows (closeGate.BeginClose spins
//     on the txInflight counter).
//  5. Capture and nil the resource pointers under db.mu.
//  6. Drain the heartbeat + flock goroutines via Coord.Close (which
//     blocks on done-channels; the flock goroutine clears writer-
//     header fields, unlocks a held writer, and fails pending
//     acquisitions).
//  7. Munmap the lock file, close the pager (data-file munmap), the
//     data-file fd, and the *os.Root.
//
// Steps 1 → 4 ordering: the CAS is the public release-store; the
// drain completes before any teardown. Steps 6 → 7 ordering: the
// SIGSEGV path the spec exists to prevent
// (final heartbeat tick on unmapped memory).
//
// Not safe to call concurrently with active write or batch
// transactions in the same process; per leak-detection.md
// §Close Ordering, callers must commit or rollback all
// transactions before Close.
//
// Close-vs-dbCleanupFn race: the shared *db.closed atomic guards
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
	// spin until quiescence. See closegate.go for the interleaving
	// analysis. Cleanups that fire AFTER this step observe
	// closed=true and skip the resource-touching work.
	//
	// We've already won the CAS — perform the drain via the same
	// gate. BeginClose stores closed=true (idempotent — already
	// true from our CAS above) and drains.
	db.closeGate.BeginClose()

	// Capture resource pointers under db.mu so a concurrent Begin
	// (which snapshots db.coord under db.mu) sees a consistent view
	// — either pre-close (non-nil) or post-nil (nil). The captured
	// locals are then used outside db.mu for the actual drain, which
	// can take milliseconds.
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

	// Step 6 — drain goroutines (step 5 was the pointer capture
	// above). Coord.Close blocks until both the
	// flock goroutine and the heartbeat goroutine have exited; with
	// a writer held at Close time, the stopCh path clears the
	// writer-header fields and issues flock(LOCK_UN) before exit.
	if coord != nil {
		_ = coord.Close()
	}
	// Step 7 — munmap the lock file. Safe: closeGate.BeginClose
	// drained Tx cleanups that might still write to lockFile's
	// mmap; Coord.Close drained heartbeat + flock goroutines that
	// also touch lockFile mmap.
	if lockFile != nil {
		_ = lockFile.Close()
	}
	// Step 7 (cont.) — release pager (munmaps data file).
	if pgr != nil {
		_ = pgr.Close()
	}
	// Step 7 (cont.) — close fds.
	if file != nil {
		_ = file.Close()
	}
	if root != nil {
		_ = root.Close()
	}

	// Cancel the DB-level leak cleanup — we closed cleanly.
	db.cleanup.Stop()
	return nil
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
// (potentially long-blocking) coord.AcquireWriter call. The first
// snapshots db.coord + db.pgr (race-safe vs Close which nil's them
// under db.mu); the second post-acquire critical section captures
// prev meta + builds the Tx + registers leak-detection cleanup. db.mu
// is NOT held across AcquireWriter — doing so would let Close
// deadlock waiting for db.mu while the coord's stopCh is closed but
// the flock goroutine still holds the result-channel send.
//
// Read transactions use the distinct BeginRead (returning *ReadTx) so
// the type system rejects write methods on a read snapshot at compile
// time (api-surface.md §Database and Transaction API).
func (db *DB) Begin(ctx context.Context) (*Tx, error) {
	// A read-only handle has no writer pager and never takes the
	// cross-process write lock — reject before any close/poison/lock
	// work (api-surface.md §Options.ReadOnly). This also covers Update,
	// which begins through here. Checked first: read-only is a
	// permanent property of the handle, so the cause is unambiguous.
	if db.readOnly {
		return nil, ErrDatabaseReadOnly
	}
	// Fast-path close check. db.closeGate.IsClosed() is the
	// spec-tier *closeGate gate (leak-detection.md §Close Ordering); a release-store at the
	// top of Close makes this Load-true any time after Close begins.
	// The subsequent snapshot under db.mu is still required: a Begin
	// that interleaves between this Load and Close's CAS sees
	// closed==false here but a nil coord after the snapshot — same
	// ErrClosed result.
	if db.closeGate.IsClosed() {
		return nil, ErrClosed
	}

	// Poison check before acquiring the cross-process lock so a
	// poisoned handle does not block legitimate concurrent callers
	// across processes.
	if db.poisoned.Load() {
		return nil, ErrPoisoned
	}

	// Snapshot db.coord under db.mu so the read is synchronized with
	// Close (which nil's it under db.mu). coord is stable for this Tx's
	// lifetime. db.pgr is read here only for the fast pre-grant closed
	// check — it is RE-READ under the post-grant db.mu below, because
	// Compact swaps db.pgr (closing the old pager) while holding the
	// write grant: a writer that captured the old pager before blocking
	// in AcquireWriter must use the post-Compact pager, never the
	// munmap'd old one.
	db.mu.Lock()
	coord := db.coord
	pgr := db.pgr
	db.mu.Unlock()
	if coord == nil || pgr == nil {
		return nil, ErrClosed
	}

	grant, err := coord.AcquireWriter(ctx)
	if err != nil {
		if errors.Is(err, lock.ErrClosed) {
			return nil, ErrClosed
		}
		return nil, err
	}

	// Re-check poison under the grant — a concurrent commit could
	// have poisoned the handle while we were waiting.
	if db.poisoned.Load() {
		grant.Release()
		return nil, ErrPoisoned
	}
	// Data-file generation check (cross-process.md §Data-file
	// generation): a peer's Compact replaced the inode this handle
	// maps — committing would write to the unlinked file, silently
	// diverging from every other process.
	if gen := coord.DataGeneration(); gen != db.dataGeneration.Load() {
		db.poisoned.Store(true)
		db.logger.Error("gmdb: data file replaced by a peer Compact; handle poisoned — Close and re-Open",
			"cachedGeneration", db.dataGeneration.Load(), "currentGeneration", gen)
		grant.Release()
		return nil, ErrPoisoned
	}

	db.mu.Lock()
	defer db.mu.Unlock()

	// Race-with-Close: if Close ran while we were waiting in
	// AcquireWriter, db.closeGate is now closed. Release the grant
	// we just got and surface ErrClosed rather than hand back a Tx
	// against a torn-down pager.
	if db.closeGate.IsClosed() {
		grant.Release()
		return nil, ErrClosed
	}

	// Re-read pgr under the post-grant db.mu: we hold the write grant, so
	// any Compact that swapped db.pgr has fully completed (it swaps under
	// grant+db.mu and releases the grant only after). This is the pager
	// the Tx must use — the pre-grant capture may be the closed old one.
	pgr = db.pgr
	if pgr == nil {
		grant.Release()
		return nil, ErrClosed
	}

	// Re-sync the writer's in-memory state (meta, bitmap, RPL, fileSize)
	// from disk before building the tx: a peer process holding the grant
	// before us may have committed, leaving our Open-time view stale.
	// Without this a serialized cross-process writer builds on a stale root
	// (lost update), writes its meta over the slot holding the peer's newer
	// commit, and allocates from a stale bitmap (page aliasing) —
	// cross-process.md §Writer acquisition flow. Cheap no-op when the
	// on-disk active TxnID is unchanged (the common single-writer path),
	// which also preserves the in-memory lastCheckpointTxnID tracking.
	if m, active, lastCheckpoint, changed, err := pgr.Resync(db.file, db.currentMeta.TxnID); err != nil {
		// Resync leaves the pager fully unmodified on error (attachState is
		// atomic), so the handle stays usable — release the grant and surface
		// the error; no poison needed.
		grant.Release()
		return nil, fmt.Errorf("gmdb: re-sync writer state on grant: %w", mapPagerErr(err))
	} else if changed {
		db.currentMeta = m
		db.activeMetaIdx = active
		db.lastCheckpointTxnID = lastCheckpoint
	}

	prevMeta := db.currentMeta
	prevActive := db.activeMetaIdx
	lastCheckpoint := db.lastCheckpointTxnID

	// RPL reclamation bound per free-space.md §RPL Reclamation:
	// min(oldestActiveReaderTxnID, lastCheckpointTxnID). We hold
	// flock(LOCK_EX) via the grant (cross-process.md §Writer's Page
	// Reclamation), so OldestReaderTxnID's LOCK_EX precondition is satisfied;
	// it returns math.MaxUint64 with no active readers, reducing the min to
	// lastCheckpointTxnID. The bound MUST use lastCheckpointTxnID, NOT
	// prevMeta.TxnID: under SyncLazy prevMeta.TxnID runs ahead of the last
	// checkpoint, and reclaiming past the last checkpoint frees data pages a
	// still-recoverable checkpoint meta's tree references
	// (free-space.md §RPL Reclamation). Under SyncDurable/SyncDataOnly every
	// commit is a checkpoint, so lastCheckpointTxnID == prevMeta.TxnID.
	bound := min(coord.OldestReaderTxnID(), lastCheckpoint)
	pgr.SetCommitState(prevMeta.HighWaterMark, prevMeta.MaxSize, bound)
	pgr.SetSizeParams(prevMeta.GrowStep, prevMeta.MinSize)
	pgr.BeginTx()
	// Seed currentTxnID at tx-start so the LaggingReader
	// callback's Lag = currentTxnID - reclamationBound is meaningful
	// when AllocPage fires from the user path (Keyspace.Put →
	// btree.Put → pw.AllocPage). Without this, currentTxnID stays
	// 0 until Tx.AllocPage or Tx.Commit explicitly sets it — and the
	// public Keyspace ops never go through Tx.AllocPage.
	newTxnID := prevMeta.TxnID + 1
	pgr.SetCurrentTxnID(newTxnID)

	// LaggingReader wiring: pass the user's callback into the pager,
	// plus a bound-refresh closure that re-derives
	// min(coord.OldestReaderTxnID(), lastCheckpointTxnID) after Wait
	// — the SAME formula as the Begin-time bound above, checkpoint
	// term included. Refreshing against prevMeta.TxnID instead would,
	// under SyncLazy (where prevMeta runs ahead of the last
	// checkpoint), reclaim segments the last checkpoint's tree still
	// references — deterministic corruption after crash recovery
	// selects that checkpoint.
	//
	// The wrapper checks coord.OldestReaderTxnID() before invoking
	// the user callback — when no reader is active, OldestReaderTxnID
	// returns MaxUint64 and the RPL-blocked condition is a checkpoint-
	// boundary effect (segments retired by prevMeta's commit pinned
	// by `< bound` strict-inequality), not a lagging-reader case.
	// Per lock-ordering.md §Lagging Reader Handling, the callback
	// fires only when "a reader in the reader table is blocking
	// reclamation." Skip the user callback in the no-reader case and
	// return Wait so the pager falls through to file extension.
	// Surface RPL-segment corruption: reclamation quarantines a torn
	// segment and continues (free-space.md §RPL Reclamation), so the
	// corruption would otherwise be invisible until an explicit Check().
	// Log it (db.logger defaults to a discard handler) and DBStats
	// carries the count for programmatic detection.
	pgr.SetRPLCorruptCallback(func(segPageID uint64) {
		db.logger.Warn("gmdb: corrupt RPL segment quarantined during reclamation; "+
			"its pages leak until Check()/Repair reclaims them",
			"segPageID", segPageID)
	})

	if userCallback := db.opts.LaggingReader; userCallback != nil {
		const noReader = ^uint64(0)
		pgr.SetLaggingReaderCallback(func(info pager.LaggingReaderInfo) pager.LaggingReaderAction {
			if coord.OldestReaderTxnID() == noReader {
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
		})
		pgr.SetReclamationBoundRefresh(func() uint64 {
			return min(coord.OldestReaderTxnID(), lastCheckpoint)
		})
	} else {
		pgr.SetLaggingReaderCallback(nil)
		pgr.SetReclamationBoundRefresh(nil)
	}

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
	// cleanup info captures the shared *closeGate by pointer
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

// readPersistedPageSize discovers the file's PageSize via pager.DiscoverPageSize,
// retrying on the EEXIST-retry fallback (raceWindow=true) so the loser
// of an O_CREATE|O_EXCL race waits for the winner to finish Init. The
// retry catches the partial-initialisation window only; a permanent
// corruption falls through to the wrapped ErrCorrupted from
// DiscoverPageSize after the final attempt. Chunk-2's lock file
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

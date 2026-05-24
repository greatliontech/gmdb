package gmdb

import (
	"context"
	"errors"
	"fmt"
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

	pool *pager.BufPool
	opts Options

	// Cross-process coordination. lockFile is the mmap'd lock file
	// (cross-process.md §Lock File Layout); coord owns the flock +
	// heartbeat goroutines and is the only writer to the writer-
	// header fields. coord is constructed in Open and torn down by
	// Close — its lifetime is the DB handle's lifetime.
	lockFile *lock.File
	coord    *lock.Coord

	// Pager state for the currently-active reader baseline. mu guards
	// against concurrent Begin from multiple goroutines.
	mu            sync.Mutex
	currentMeta   page.Meta
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

	// closeGate is a heap-allocated coordination struct shared by
	// pointer with every txCleanupInfo, every readTxCleanupInfo, and
	// the dbCleanupInfo. Composes the (closed bool, txInflight
	// counter) pair the chunk-3.3 promotion of leak-detection.md
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
func Open(_ context.Context, path string, opts Options) (*DB, error) {
	opts = opts.applyDefaults()
	if err := opts.validate(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidOptions, err)
	}
	dir := filepath.Dir(path)
	base := filepath.Base(path)
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("gmdb: open dir %q: %w", dir, err)
	}
	// Per api-surface.md §Database Initialization: try O_CREATE|O_EXCL
	// first and on EEXIST fall back to the normal-open path. This is
	// the correct ordering against a concurrent Open race — the
	// loser observes EEXIST and proceeds as a regular open of the
	// just-created file.
	file, err := root.OpenFile(base, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	created := true
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

	// Read the on-disk meta-0 PageSize so the pool matches the
	// persisted size — opts.PageSize is authoritative only at
	// creation; re-opens honour what was written.
	//
	// If we took the EEXIST-retry fallback, another process may still
	// be in the middle of pager.Init (file exists but metas not yet
	// written). Read meta-0 with bounded retry-on-empty so we don't
	// observe a half-initialised file and fail with a misleading
	// "PageSize invalid" error. Chunk 2's lock file resolves the
	// race structurally; until then this is a chunk-1 defensive
	// measure.
	persistedPageSize, err := readPersistedPageSize(file, !created)
	if err != nil {
		_ = file.Close()
		_ = root.Close()
		return nil, err
	}
	pool := pager.NewBufPool(int(persistedPageSize))
	opened, err := pager.Open(file, pager.OpenParams{
		Pool:             pool,
		MaxTxBufferBytes: opts.MaxTxBufferBytes,
	})
	if err != nil {
		_ = file.Close()
		_ = root.Close()
		return nil, mapPagerErr(err)
	}

	// durability.md §Recovery step 3: recovery accepted a
	// non-checkpoint meta because no checkpoint-flagged meta exists
	// (SyncLazy-only DB never Checkpoint()'d). Warn — data
	// integrity depends on whether the OS flushed pages in the
	// right order, which is not guaranteed.
	if opened.NoCheckpoint {
		slog.Default().Warn(
			"gmdb: Open accepted non-checkpoint meta",
			"path", path,
			"txn_id", opened.Meta.TxnID,
			"detail", "no checkpoint-flagged meta found; data integrity depends on OS flush ordering",
		)
	}

	// Open the lock file under the same os.Root — symlink-escape
	// protection is shared with the data file. The DataUUID is the
	// just-opened pager's meta UUID; a stale lock file with a
	// different UUID is unlinked-and-recreated by lock.Open.
	lockFile, err := lock.Open(lock.OpenParams{
		Root:       root,
		Base:       lock.BaseFor(base),
		DataUUID:   opened.Meta.UUID,
		MaxReaders: opts.MaxReaders,
	})
	if err != nil {
		_ = opened.Pager.Close()
		_ = file.Close()
		_ = root.Close()
		return nil, mapLockErr(err)
	}

	// Cache this process's identity once. Failures (no /proc,
	// hardened sandbox, non-Linux ProcessStartTime stub) surface as
	// 0 — the protocol routes through the heartbeat path when either
	// value is unavailable. logging is not yet wired (Options.Logger
	// arrives with the chunk that needs it).
	processStartTime, _ := lock.ProcessStartTime(os.Getpid())
	pidNamespace, _ := lock.PIDNamespace()

	coord := lock.NewCoord(lockFile, lock.CoordOptions{
		PID:              uint64(os.Getpid()),
		ProcessStartTime: processStartTime,
		PIDNamespace:     pidNamespace,
		// RetryInterval / HeartbeatInterval default to the lock-package
		// constants (cross-process.md). Exposed via Options when a
		// caller needs to tune them.
	})

	db := &DB{
		file:          file,
		root:          root,
		pool:          pool,
		opts:          opts,
		lockFile:      lockFile,
		coord:         coord,
		currentMeta:   opened.Meta,
		activeMetaIdx: opened.ActiveMetaIdx,
		pgr:           opened.Pager,
		closeGate:     newCloseGate(),
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
		originPCs: captureOriginPCs(),
	})
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
	slog.Default().Warn(
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
//  1. Store `*db.closed = true` (release-store on the shared
//     *atomic.Bool). Visible to any subsequent Tx cleanup callback
//     regardless of runtime.AddCleanup ordering between the DB and
//     its Txs — they observe the close state and exit without
//     touching torn-down resources.
//  2. Drain the heartbeat + flock goroutines via Coord.Close (which
//     blocks on done-channels). The flock goroutine clears writer-
//     header fields and unlocks if a writer was held at Close time;
//     the heartbeat goroutine exits before any unmap.
//  3. Munmap the lock file. Safe only after step 2.
//  4. Close pager (munmaps data file).
//  5. Close data-file fd.
//  6. Close *os.Root.
//
// Steps 1 → 2 ordering: ANY swap is the public release-store. Steps
// 2 → 3 ordering: the SIGSEGV path the spec exists to prevent
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
	// Step 1a — release-store closed=true via gate.CAS. Idempotency:
	// a second Close returns immediately because only one caller
	// wins the CAS.
	if !db.closeGate.CompareAndSwapClosed(false, true) {
		return nil
	}
	// Step 1b — drain in-flight Tx cleanups. Cleanups that observed
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

	// Step 2 — drain goroutines. Coord.Close blocks until both the
	// flock goroutine and the heartbeat goroutine have exited; with
	// a writer held at Close time, the stopCh path clears the
	// writer-header fields and issues flock(LOCK_UN) before exit.
	if coord != nil {
		_ = coord.Close()
	}
	// Step 3 — munmap the lock file. Safe: closeGate.BeginClose
	// drained Tx cleanups that might still write to lockFile's
	// mmap; Coord.Close drained heartbeat + flock goroutines that
	// also touch lockFile mmap.
	if lockFile != nil {
		_ = lockFile.Close()
	}
	// Step 4 — release pager (munmaps data file).
	if pgr != nil {
		_ = pgr.Close()
	}
	// Steps 5–6 — close fds.
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

// Meta returns a snapshot of the currently-active meta. Useful for
// tests; the user-facing Stats API arrives in chunk 11.
func (db *DB) Meta() page.Meta {
	db.mu.Lock()
	defer db.mu.Unlock()
	return db.currentMeta
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
// Per the chunk-3 spec amend in api-surface.md, read and write
// transactions live in distinct types — write goes through Begin
// (returning *Tx); read goes through BeginRead (returning *ReadTx).
// The legacy `Begin(ctx, false)` path is preserved as a hard error
// (ErrReadOnly) so existing callers fail loud rather than silently
// pass the wrong-typed handle to write-only code; the spec amend
// removed the unified-Tx writable=false case entirely.
func (db *DB) Begin(ctx context.Context, write bool) (*Tx, error) {
	if !write {
		return nil, ErrReadOnly
	}
	// Fast-path close check. db.closeGate.IsClosed() is the
	// spec-tier *closeGate gate (leak-detection.md §Close Ordering
	// + chunk-3.3 refcount-drain promotion); a release-store at the
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

	// Snapshot db.coord + db.pgr under db.mu so the read is
	// synchronized with Close (which nil's both under db.mu). The
	// captured pointers are stable for this Tx's lifetime
	// (independent of subsequent Close calls that nil the *DB
	// fields).
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

	prevMeta := db.currentMeta
	prevActive := db.activeMetaIdx

	// Compute the RPL reclamation bound per free-space.md §RPL
	// Reclamation: min(oldestActiveReaderTxnID, lastCheckpointTxnID).
	// We hold flock(LOCK_EX) via the grant (cross-process.md
	// §Writer's Page Reclamation), so OldestReaderTxnID's
	// LOCK_EX precondition is satisfied. In chunk 3.4 (SyncMode
	// not yet wired — that's 3.5), every commit is a checkpoint,
	// so lastCheckpointTxnID == prevMeta.TxnID; the SyncLazy split
	// arrives in 3.5 alongside the MetaFlagCheckpoint-bit
	// interpretation. OldestReaderTxnID returns math.MaxUint64
	// when no readers are active, which the `min` reduces to
	// lastCheckpointTxnID — the old chunk-2 behaviour preserved
	// when no readers exist.
	bound := min(coord.OldestReaderTxnID(), prevMeta.TxnID)
	pgr.SetCommitState(prevMeta.HighWaterMark, prevMeta.MaxSize, bound)
	pgr.BeginTx()

	held := &atomic.Bool{}
	held.Store(true)
	tx := &Tx{
		db:           db,
		pgr:          pgr,
		prevMeta:     prevMeta,
		prevActive:   prevActive,
		newTxnID:     prevMeta.TxnID + 1,
		writable:     true,
		held:         held,
		grant:        grant,
		keyspaceRoot: prevMeta.KeyspaceRoot,
		numKeyspaces: prevMeta.NumKeyspaces,
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
// chunk-1 defensive measure.
func readPersistedPageSize(file *os.File, raceWindow bool) (uint32, error) {
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
		if i+1 < attempts {
			time.Sleep(backoff)
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
	tx, err := db.Begin(ctx, true)
	if err != nil {
		return err
	}
	if fnErr := fn(tx); fnErr != nil {
		if rbErr := tx.Rollback(); rbErr != nil {
			return errors.Join(fnErr, rbErr)
		}
		return fnErr
	}
	return tx.Commit()
}

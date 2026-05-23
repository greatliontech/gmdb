package gmdb

import (
	"context"
	"errors"
	"fmt"
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

	return &DB{
		file:          file,
		root:          root,
		pool:          pool,
		opts:          opts,
		lockFile:      lockFile,
		coord:         coord,
		currentMeta:   opened.Meta,
		activeMetaIdx: opened.ActiveMetaIdx,
		pgr:           opened.Pager,
	}, nil
}

// Close releases all resources held by the DB handle. After Close, the
// handle is unusable; subsequent Begin returns ErrClosed.
//
// Resource teardown order (cross-process.md §Heartbeat Goroutine
// "Shutdown coordination" + the Close-releases clause-explicit
// invariant):
//
//  1. Coord.Close — drains the flock + heartbeat goroutines. The
//     flock goroutine clears writer-header fields and unlocks if a
//     writer was held at Close time. The heartbeat goroutine exits
//     before the *File becomes unmappable. Both close-acks are
//     synchronously awaited.
//  2. lock.File.Close — munmaps the lock file and closes its fd.
//     Safe only after step 1: a final heartbeat tick must not race
//     the munmap (SIGSEGV).
//  3. pager.Close — releases data-file mmap.
//  4. data file close + root close.
//
// Reversing 1 and 2 (close the lock file before draining the Coord
// goroutines) is the classic SIGSEGV path the spec invariant
// exists to prevent.
func (db *DB) Close() error {
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.coord != nil {
		_ = db.coord.Close()
		db.coord = nil
	}
	if db.lockFile != nil {
		_ = db.lockFile.Close()
		db.lockFile = nil
	}
	if db.pgr != nil {
		_ = db.pgr.Close()
		db.pgr = nil
	}
	if db.file != nil {
		_ = db.file.Close()
		db.file = nil
	}
	if db.root != nil {
		_ = db.root.Close()
		db.root = nil
	}
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
// Read transactions are not yet wired (chunk 3 territory); calling
// Begin(write=false) returns ErrReadOnly to signal the unimplemented
// path explicitly.
func (db *DB) Begin(ctx context.Context, write bool) (*Tx, error) {
	if !write {
		return nil, ErrReadOnly
	}
	// Poison check before acquiring the cross-process lock so a
	// poisoned handle does not block legitimate concurrent callers
	// across processes.
	if db.poisoned.Load() {
		return nil, ErrPoisoned
	}

	// Snapshot db.coord under db.mu so the read is synchronized with
	// Close (which nil's db.coord under db.mu). If Close has already
	// run, coord is nil — return ErrClosed without entering the
	// goroutine path. If Close runs concurrently AFTER our snapshot,
	// coord points to an already-closed Coord; its AcquireWriter
	// returns lock.ErrClosed via the stopCh path, which we map below.
	// Chunk 2.8 promotes this to a single db.closed atomic.Bool with
	// a tighter ordering contract.
	db.mu.Lock()
	coord := db.coord
	db.mu.Unlock()
	if coord == nil {
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
	// AcquireWriter, the goroutine may have returned a grant moments
	// before the stopCh path cleaned up — grant.Release on a Coord
	// whose goroutine has exited is a no-op (channel close against
	// nothing), but we still need to surface ErrClosed rather than
	// hand back a Tx against a torn-down pager.
	if db.pgr == nil {
		grant.Release()
		return nil, ErrClosed
	}

	prevMeta := db.currentMeta
	prevActive := db.activeMetaIdx
	db.pgr.BeginTx()

	held := &atomic.Bool{}
	held.Store(true)
	tx := &Tx{
		db:         db,
		prevMeta:   prevMeta,
		prevActive: prevActive,
		newTxnID:   prevMeta.TxnID + 1,
		writable:   true,
		held:       held,
		grant:      grant,
	}
	// Wire the leak-detection cleanup per leak-detection.md. The
	// cleanup info captures *Pager, *Grant, the shared held atomic,
	// and the origin stack — never the *Tx itself
	// (resurrection-forbidden).
	tx.cleanup = runtime.AddCleanup(tx, txCleanupFn, txCleanupInfo{
		pgr:       db.pgr,
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

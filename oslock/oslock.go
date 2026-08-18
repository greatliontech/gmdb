// Package oslock provides advisory file locks with process-lifetime
// binding: a Lock is one open file description holding the exclusive
// advisory lock on a lock file, released by the kernel when the
// holder's last reference closes — process death included, SIGKILL
// included. That binding makes a held lock a liveness witness: a
// non-blocking acquisition that succeeds proves the previous holder
// is gone, and the acquisition itself is the successor's claim — one
// atomic act, no window between judging and acting. A blocked
// acquisition proves the holder is alive (a frozen process still
// holds). A TryAcquire that errors any other way is UNDECIDED —
// wrapped in ErrUndecided, never a death verdict; callers retry
// later. (Acquire renders no verdict; its errors are ordinary
// acquisition failures. Classify causes with errors.Is against
// fs.ErrPermission and friends — the deprecated os.IsPermission
// predicates do not see through wrapped errors.)
//
// Verdicts are sound only within one locking domain — the set of
// openers among whom the filesystem makes advisory locks conflict.
// One host's processes opening the same filesystem are one domain
// whatever their namespaces (locks are inode-scoped, so bind mounts
// share them); network filesystems with client-local locks, and
// stacked views (an overlay upper, a passthrough FUSE view), are
// different domains and get no sound verdicts. Callers for whom a
// wrong verdict is destructive should probe the filesystem at
// startup: hold a lock and try-acquire it through a second
// descriptor — the try must observe contention.
//
// Descriptors are opened close-on-exec and are never placed on a
// child's inheritance list by this package, so an exec'd child
// cannot carry a claim past its parent. A fork that never execs
// shares the description until it exits — a bounded false-live in
// the safe direction.
//
// Acquisition verifies identity after locking: the locked
// descriptor's file is compared against the path's, and a mismatch —
// the file was unlinked and recreated underfoot — closes and
// retries. The companion discipline for holders: a claim whose
// meaning has ended is retired with Retire, which unlinks the lock
// file while the lock is still held and only then releases —
// unlink-after-release would race a fresh acquirer of the old inode
// into a second-holder state the identity verify cannot catch. Close
// alone releases without unlinking (a deferral: the claim outlives
// this holder, as when handing off or backing out).
package oslock

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync/atomic"
	"time"

	"github.com/greatliontech/gmdb/internal/flock"
)

// ErrHeld reports a non-blocking acquisition that observed a live
// holder: the verdict "alive".
var ErrHeld = errors.New("oslock: lock held by a live process")

// ErrUndecided wraps every TryAcquire error that is not ErrHeld:
// the try could not judge — a transient open failure, a permission
// problem, a churned path — and the caller retries later, never
// reading it as death. Verdict consumers branch on exactly three
// outcomes: nil (dead, and claimed), ErrHeld (alive), ErrUndecided
// (no verdict).
var ErrUndecided = errors.New("oslock: verdict undecided")

// ErrUnlinkDeferred reports a Retire whose unlink the platform
// refused (open files it will not unlink): the release still
// happened and the leftover file is an acquirable dead claim — a
// normal branch, not a failure of the retirement.
var ErrUnlinkDeferred = errors.New("oslock: unlink deferred")

// ErrRetired reports Retire on an already-closed Lock — a
// programming error: a closed Lock must never unlink what may now
// be a successor's live lock file.
var ErrRetired = errors.New("oslock: lock already closed")

// acquirePollInterval paces Acquire's cancellable poll. Polling —
// never a blocking flock — keeps cancellation free of abandoned
// goroutines, descriptors, and kernel waiters (the accumulation
// pathology cross-process.md §Write Lock rejects); one probe per
// interval bounds the acquisition latency after a release.
const acquirePollInterval = 10 * time.Millisecond

// afterLockHook runs between acquisition and the identity verify —
// the deterministic seam for driving the unlink-recreate race the
// verify exists to catch. Atomic because acquisitions on other
// goroutines may still be in flight when a test swaps it.
var afterLockHook atomic.Pointer[func(path string)]

// setAfterLockHookForTest installs the seam and returns its restore
// function. Never called outside tests.
func setAfterLockHookForTest(h func(path string)) (restore func()) {
	afterLockHook.Store(&h)
	return func() { afterLockHook.Store(nil) }
}

// Lock is a held exclusive advisory lock: one open file description
// on the lock file, constructed only by a successful acquisition.
// A Lock belongs to one owner; its methods are not safe for
// concurrent use.
type Lock struct {
	f      *os.File
	path   string
	closed bool
}

// open opens the lock file, creating it if absent. The descriptor is
// close-on-exec (Go's default open semantics on every platform).
func open(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
}

// verified reports whether f still is the file the path names — a
// mismatch means the file was unlinked (and possibly recreated)
// between open and lock, and the acquisition must retry on the
// path's current file. The comparison cannot alias a recycled
// identity: the open descriptor pins its file against reuse until
// close.
func verified(f *os.File, path string) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	pi, err := os.Stat(path)
	if err != nil {
		return false
	}
	return os.SameFile(fi, pi)
}

// acquireVerified is the shared acquisition skeleton: open the path
// (creating if absent), take the lock via lock, and keep the
// acquisition only if the descriptor still is the path's file —
// otherwise close and retry on the current file. openErr disposes
// an open failure and retry gates each verify-mismatch iteration:
// a non-nil return from either surfaces it, nil continues.
func acquireVerified(path string, openErr func(error) error, lock func(fd uintptr) error, retry func() error) (*Lock, error) {
	for {
		f, err := open(path)
		if err != nil {
			if err := openErr(err); err != nil {
				return nil, err
			}
			continue
		}
		if err := lock(f.Fd()); err != nil {
			f.Close()
			return nil, err
		}
		if hp := afterLockHook.Load(); hp != nil {
			(*hp)(path)
		}
		if !verified(f, path) {
			f.Close()
			if err := retry(); err != nil {
				return nil, err
			}
			continue
		}
		return &Lock{f: f, path: path}, nil
	}
}

// TryAcquire attempts the exclusive lock on path without blocking.
// The verdict is three-valued: ErrHeld means a live holder has the
// lock; success is both the verdict that no holder survives and the
// caller's own claim; every other error wraps ErrUndecided — the
// try could not judge (a transient open failure, a permission
// problem, a churned path) and the caller retries later, never
// treating it as death. The lock file is created if absent.
func TryAcquire(path string) (*Lock, error) {
	verifyRetries := 0
	return acquireVerified(path,
		func(err error) error { return fmt.Errorf("%w: %w", ErrUndecided, err) },
		func(fd uintptr) error {
			for {
				err := flock.TryExclusive(fd)
				if flock.ErrRetryable(err) {
					continue
				}
				if flock.ErrContended(err) {
					return fmt.Errorf("%w: %s", ErrHeld, path)
				}
				if err != nil {
					return fmt.Errorf("%w: %w", ErrUndecided, err)
				}
				return nil
			}
		},
		func() error {
			// A foreign writer churning the path (unlink+recreate in
			// a loop) must not spin a never-blocking call forever:
			// exhaustion is an undecided verdict, never death.
			verifyRetries++
			if verifyRetries > 100 {
				return fmt.Errorf("%w: lock file churning underfoot: %s", ErrUndecided, path)
			}
			return nil
		})
}

// Acquire waits for the exclusive lock on path or ctx. The wait
// polls a non-blocking probe: cancellation leaves zero goroutines,
// descriptors, or abandoned kernel waiters behind, and acquisition
// follows a release within one poll interval. A transient open
// failure (a just-unlinked file in delete-pending state on windows)
// heals within moments and is retried against a bounded per-call
// budget (on the order of 100ms); a persistent one (permissions, a
// missing parent) surfaces as its error.
func Acquire(ctx context.Context, path string) (*Lock, error) {
	openFailures := 0
	return acquireVerified(path,
		func(err error) error {
			openFailures++
			if openFailures > 100 {
				return err
			}
			select {
			case <-ctx.Done():
				return context.Cause(ctx)
			case <-time.After(time.Millisecond):
				return nil
			}
		},
		func(fd uintptr) error {
			return flock.ExclusiveCtx(ctx, fd, acquirePollInterval)
		},
		func() error {
			// Verify-mismatch churn stays cancellable even when every
			// probe acquires instantly (ExclusiveCtx never consulted
			// ctx on a first-probe success) — and paced: an
			// uncancellable context must not turn churn into a
			// busy-spin.
			select {
			case <-ctx.Done():
				return context.Cause(ctx)
			case <-time.After(acquirePollInterval):
				return nil
			}
		})
}

// Path returns the lock file's path.
func (l *Lock) Path() string { return l.path }

// Retire ends the claim this lock witnesses: the lock file is
// unlinked while the lock is still held, and only then is the lock
// released — the ordering that keeps a fresh acquirer off the
// retired inode. An unlink refusal (a platform that will not unlink
// open files) is returned as a deferral after the release still
// happens — the lock, not the file's absence, is the authority, and
// an unheld leftover file is an acquirable dead claim, not a hazard.
// Retire after Close is an error and touches nothing.
func (l *Lock) Retire() error {
	if l.closed {
		return ErrRetired
	}
	unlinkErr := os.Remove(l.path)
	closeErr := l.f.Close()
	l.closed = true
	if unlinkErr != nil {
		unlinkErr = fmt.Errorf("%w: %w", ErrUnlinkDeferred, unlinkErr)
	}
	return errors.Join(unlinkErr, closeErr)
}

// Close releases the lock and the descriptor without unlinking the
// lock file — the claim's name persists for the next acquirer. The
// kernel releases the lock with the last reference regardless of
// the error.
func (l *Lock) Close() error {
	if l.closed {
		return nil
	}
	l.closed = true
	return l.f.Close()
}

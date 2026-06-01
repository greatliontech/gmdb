package lock

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// newTestCoord opens a fresh *File via tmpLock and wraps it in a
// Coord with deterministic identity values (PID=4242 / StartTime=7 /
// PIDNamespace=99). Returns the Coord, the *File (so tests can inspect
// header fields directly), and registers Close on t.Cleanup.
func newTestCoord(t *testing.T, retry time.Duration) (*Coord, *File) {
	t.Helper()
	root, base, _ := tmpLock(t)
	f, err := Open(OpenParams{
		Root: root, Base: base, DataUUID: [16]byte{0xC0, 0x0D}, MaxReaders: 8,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	c := NewCoord(f, CoordOptions{
		PID:              4242,
		ProcessStartTime: 7,
		PIDNamespace:     99,
		RetryInterval:    retry,
	})
	t.Cleanup(func() {
		_ = c.Close()
		_ = f.Close()
	})
	return c, f
}

func TestCoordGrantAndRelease(t *testing.T) {
	c, f := newTestCoord(t, 10*time.Millisecond)

	grant, err := c.AcquireWriter(context.Background())
	if err != nil {
		t.Fatalf("AcquireWriter: %v", err)
	}
	if grant == nil {
		t.Fatal("AcquireWriter returned nil grant + nil err")
	}

	// Header must be published under the grant.
	if got := f.WriterPID(); got != 4242 {
		t.Errorf("WriterPID under grant = %d, want 4242", got)
	}
	if got := f.WriterStartTime(); got != 7 {
		t.Errorf("WriterStartTime under grant = %d, want 7", got)
	}
	if got := f.WriterPIDNamespace(); got != 99 {
		t.Errorf("WriterPIDNamespace under grant = %d, want 99", got)
	}

	grant.Release()

	// After Release the goroutine asynchronously clears + unlocks.
	// Poll briefly for the header to clear; the clear is bounded by
	// one channel hop + four atomic stores + one Flock syscall, so a
	// 200 ms ceiling is generous.
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		if f.WriterPID() == 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if got := f.WriterPID(); got != 0 {
		t.Errorf("WriterPID after Release = %d, want 0", got)
	}
	if got := f.WriterStartTime(); got != 0 {
		t.Errorf("WriterStartTime after Release = %d, want 0", got)
	}
	if got := f.WriterPIDNamespace(); got != 0 {
		t.Errorf("WriterPIDNamespace after Release = %d, want 0", got)
	}
}

func TestCoordReleaseIdempotent(t *testing.T) {
	c, _ := newTestCoord(t, 10*time.Millisecond)
	grant, err := c.AcquireWriter(context.Background())
	if err != nil {
		t.Fatalf("AcquireWriter: %v", err)
	}
	grant.Release()
	grant.Release() // second call must not panic (sync.Once)
}

func TestCoordSerialisesGoroutines(t *testing.T) {
	// Same-Coord intra-process serialisation: N goroutines call
	// AcquireWriter on the same Coord; at most one grant is held at
	// a time. The serialiser here is the unbuffered writerCh + the
	// single run() goroutine — the test pins that channel pattern,
	// not the cross-OFD flock-as-serialiser claim. For the
	// cross-OFD test see TestCoordFlockSerialisesAcrossCoords.
	c, f := newTestCoord(t, time.Millisecond)

	const N = 8
	var concurrent atomic.Int32
	var maxConcurrent atomic.Int32
	var wg sync.WaitGroup
	wg.Add(N)
	for range N {
		go func() {
			defer wg.Done()
			grant, err := c.AcquireWriter(context.Background())
			if err != nil {
				t.Errorf("AcquireWriter: %v", err)
				return
			}
			now := concurrent.Add(1)
			for {
				m := maxConcurrent.Load()
				if now <= m || maxConcurrent.CompareAndSwap(m, now) {
					break
				}
			}
			// Hold briefly so a (broken) parallel grant has a window
			// to show up.
			time.Sleep(2 * time.Millisecond)
			if f.WriterPID() != 4242 {
				t.Errorf("WriterPID during hold = %d, want 4242", f.WriterPID())
			}
			concurrent.Add(-1)
			grant.Release()
		}()
	}
	wg.Wait()

	if got := maxConcurrent.Load(); got != 1 {
		t.Errorf("max concurrent grants = %d, want 1 (single-goroutine flock invariant)", got)
	}
}

func TestCoordCtxCancelBeforeSubmit(t *testing.T) {
	c, _ := newTestCoord(t, 10*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.AcquireWriter(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("got %v, want context.Canceled", err)
	}
}

func TestCoordCtxCancelDuringEWOULDBLOCK(t *testing.T) {
	// Hold the lock with a foreground grant, then have a second
	// goroutine call AcquireWriter with a ctx that fires before the
	// hold is released. The blocked acquirer must return promptly
	// (within one retry interval after cancel) and the held grant
	// must release normally.
	c, _ := newTestCoord(t, 10*time.Millisecond)

	hold, err := c.AcquireWriter(context.Background())
	if err != nil {
		t.Fatalf("hold AcquireWriter: %v", err)
	}
	defer hold.Release()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, e := c.AcquireWriter(ctx)
		done <- e
	}()

	// Let the blocked goroutine enter its EWOULDBLOCK retry loop.
	time.Sleep(30 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("got %v, want context.Canceled", err)
		}
	case <-time.After(200 * time.Millisecond):
		t.Errorf("blocked AcquireWriter did not return within 200 ms after ctx cancel")
	}
}

func TestCoordCloseWhileHolding(t *testing.T) {
	// Spec invariant "Close-releases" (cross-process.md §Invariants):
	// if a writer holds the lock at Close time, the goroutine must
	// clear the header + release flock(LOCK_UN) before Close returns.
	// Otherwise the next opener (in another process) would block
	// indefinitely on flock(LOCK_EX|LOCK_NB) retries.
	//
	// Verifying flock release requires a DIFFERENT open-file
	// description than the one the Coord holds — flock is keyed on
	// the OFD, and a same-OFD LOCK_EX|LOCK_NB against a held LOCK_EX
	// is documented as a no-op mode conversion (succeeds whether or
	// not the original lock was released). We use a second *File on
	// the same inode as the witness fd; its LOCK_EX|LOCK_NB must
	// EWOULDBLOCK while the Coord holds, and succeed after Close.
	root, base, _ := tmpLock(t)
	uuid := [16]byte{0xC1}
	f, err := Open(OpenParams{Root: root, Base: base, DataUUID: uuid, MaxReaders: 4})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	witness, err := Open(OpenParams{Root: root, Base: base, DataUUID: uuid, MaxReaders: 4})
	if err != nil {
		t.Fatalf("Open witness: %v", err)
	}
	defer witness.Close()

	c := NewCoord(f, CoordOptions{PID: 1, ProcessStartTime: 2, PIDNamespace: 3,
		RetryInterval: 10 * time.Millisecond})

	grant, err := c.AcquireWriter(context.Background())
	if err != nil {
		t.Fatalf("AcquireWriter: %v", err)
	}

	// Sanity: header is published under the grant.
	if got := f.WriterPID(); got != 1 {
		t.Fatalf("WriterPID under grant = %d, want 1", got)
	}

	// Pre-Close, the witness fd must NOT be able to acquire LOCK_EX —
	// the Coord is holding flock on a different OFD for the same file.
	if err := syscall.Flock(int(witness.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
		t.Fatalf("witness flock succeeded while Coord holds — Coord did not actually take LOCK_EX")
	} else if !errors.Is(err, syscall.EWOULDBLOCK) {
		t.Fatalf("witness flock pre-Close: got %v, want EWOULDBLOCK", err)
	}

	// Close while still holding. Coord must clear the header and
	// release flock before returning.
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	_ = grant // Grant is now stale; Release is a no-op against a
	// no-longer-listening goroutine.

	// Header cleared by the stopCh path.
	if got := f.WriterPID(); got != 0 {
		t.Errorf("WriterPID after Close-while-held = %d, want 0", got)
	}
	if got := f.WriterStartTime(); got != 0 {
		t.Errorf("WriterStartTime after Close-while-held = %d, want 0", got)
	}
	if got := f.WriterPIDNamespace(); got != 0 {
		t.Errorf("WriterPIDNamespace after Close-while-held = %d, want 0", got)
	}

	// Post-Close, the witness fd MUST now be able to acquire LOCK_EX —
	// proves the Coord released the kernel flock on its stopCh path.
	if err := syscall.Flock(int(witness.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Errorf("witness flock post-Close: got %v, want success (Close-releases invariant)", err)
	} else {
		// Defer LOCK_UN immediately on success so a subsequent
		// t.Fatal/t.FailNow cannot leak the flock past the test.
		defer func() { _ = syscall.Flock(int(witness.Fd()), syscall.LOCK_UN) }()
	}
	_ = f.Close()
}

func TestCoordCloseWhileAcquiring(t *testing.T) {
	// Coord holds the lock; second AcquireWriter is in EWOULDBLOCK
	// retry; Close fires. Both Close and the blocked AcquireWriter
	// must return promptly.
	c, _ := newTestCoord(t, 10*time.Millisecond)

	hold, err := c.AcquireWriter(context.Background())
	if err != nil {
		t.Fatalf("hold AcquireWriter: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, e := c.AcquireWriter(context.Background())
		done <- e
	}()
	time.Sleep(30 * time.Millisecond) // let it enter retry loop

	closeErr := make(chan error, 1)
	go func() { closeErr <- c.Close() }()

	select {
	case err := <-done:
		if !errors.Is(err, ErrClosed) {
			t.Errorf("blocked AcquireWriter returned %v, want ErrClosed", err)
		}
	case <-time.After(300 * time.Millisecond):
		t.Errorf("blocked AcquireWriter did not return within 300 ms after Close")
	}

	select {
	case err := <-closeErr:
		if err != nil {
			t.Errorf("Close: %v", err)
		}
	case <-time.After(300 * time.Millisecond):
		t.Errorf("Close did not return within 300 ms")
	}

	// hold.Release on a closed Coord is a no-op (channel close against
	// nothing).
	hold.Release()
}

func TestCoordAcquireOnClosedReturnsErrClosed(t *testing.T) {
	c, _ := newTestCoord(t, 10*time.Millisecond)
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	_, err := c.AcquireWriter(context.Background())
	if !errors.Is(err, ErrClosed) {
		t.Errorf("got %v, want ErrClosed", err)
	}
}

func TestCoordCloseIdempotent(t *testing.T) {
	c, _ := newTestCoord(t, 10*time.Millisecond)
	if err := c.Close(); err != nil {
		t.Errorf("first Close: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

func TestCoordDefaultRetryInterval(t *testing.T) {
	// Zero RetryInterval → defaultRetryInterval. Functional check: a
	// grant + release round-trip works without specifying retry.
	root, base, _ := tmpLock(t)
	f, err := Open(OpenParams{
		Root: root, Base: base, DataUUID: [16]byte{0xC2}, MaxReaders: 4,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	c := NewCoord(f, CoordOptions{PID: 1}) // RetryInterval = 0
	t.Cleanup(func() {
		_ = c.Close()
		_ = f.Close()
	})
	grant, err := c.AcquireWriter(context.Background())
	if err != nil {
		t.Fatalf("AcquireWriter: %v", err)
	}
	grant.Release()
}

func TestCoordCrossCoordBlockingHandoff(t *testing.T) {
	// Two Coords on the same lock file simulate two processes sharing
	// one lock file. AcquireWriter on Coord B must block until Coord
	// A releases, then promptly hand off. Pins cross-OFD flock
	// blocking + release-handoff timing. Distinct from
	// TestCoordSerialisesGoroutines (intra-Coord channel pattern) and
	// from TestCoordFlockSerialisesAcrossCoords (max-concurrent
	// invariant under N goroutines).
	root, base, _ := tmpLock(t)
	uuid := [16]byte{0xC3}

	fA, err := Open(OpenParams{Root: root, Base: base, DataUUID: uuid, MaxReaders: 4})
	if err != nil {
		t.Fatalf("Open A: %v", err)
	}
	defer fA.Close()
	fB, err := Open(OpenParams{Root: root, Base: base, DataUUID: uuid, MaxReaders: 4})
	if err != nil {
		t.Fatalf("Open B: %v", err)
	}
	defer fB.Close()

	cA := NewCoord(fA, CoordOptions{PID: 11, RetryInterval: 10 * time.Millisecond})
	defer cA.Close()
	cB := NewCoord(fB, CoordOptions{PID: 22, RetryInterval: 10 * time.Millisecond})
	defer cB.Close()

	gA, err := cA.AcquireWriter(context.Background())
	if err != nil {
		t.Fatalf("cA.AcquireWriter: %v", err)
	}

	// Header reflects cA.
	if got := fB.WriterPID(); got != 11 {
		t.Errorf("WriterPID via fB while A holds = %d, want 11", got)
	}

	// cB.AcquireWriter must block until cA releases. Verify by
	// racing a release after a delay.
	done := make(chan error, 1)
	go func() {
		grant, err := cB.AcquireWriter(context.Background())
		if grant != nil {
			grant.Release()
		}
		done <- err
	}()

	time.Sleep(40 * time.Millisecond)
	select {
	case <-done:
		t.Fatalf("cB.AcquireWriter returned while cA still holds")
	default:
	}

	gA.Release()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("cB.AcquireWriter: %v", err)
		}
	case <-time.After(300 * time.Millisecond):
		t.Errorf("cB.AcquireWriter did not return within 300 ms after cA release")
	}
}

func TestCoordFlockSerialisesAcrossCoords(t *testing.T) {
	// Spec invariant 7 (single-goroutine flock) is enforced
	// process-wide. To pin that flock() itself — not just a single
	// channel — is the serialiser, race N goroutines split across
	// TWO Coords on the same lock file. Each Coord runs its own
	// run() loop and goroutine, so the only thing keeping max-
	// concurrent grants at 1 is the kernel's flock(LOCK_EX) on the
	// underlying inode.
	root, base, _ := tmpLock(t)
	uuid := [16]byte{0xC4}

	fA, err := Open(OpenParams{Root: root, Base: base, DataUUID: uuid, MaxReaders: 4})
	if err != nil {
		t.Fatalf("Open A: %v", err)
	}
	defer fA.Close()
	fB, err := Open(OpenParams{Root: root, Base: base, DataUUID: uuid, MaxReaders: 4})
	if err != nil {
		t.Fatalf("Open B: %v", err)
	}
	defer fB.Close()

	cA := NewCoord(fA, CoordOptions{PID: 11, RetryInterval: time.Millisecond})
	defer cA.Close()
	cB := NewCoord(fB, CoordOptions{PID: 22, RetryInterval: time.Millisecond})
	defer cB.Close()

	const N = 8 // 4 per Coord
	var concurrent atomic.Int32
	var maxConcurrent atomic.Int32
	var wg sync.WaitGroup
	wg.Add(N)
	acquire := func(c *Coord) {
		defer wg.Done()
		grant, err := c.AcquireWriter(context.Background())
		if err != nil {
			t.Errorf("AcquireWriter: %v", err)
			return
		}
		now := concurrent.Add(1)
		for {
			m := maxConcurrent.Load()
			if now <= m || maxConcurrent.CompareAndSwap(m, now) {
				break
			}
		}
		// Hold briefly so a (broken) parallel grant from the OTHER
		// Coord would race in here.
		time.Sleep(2 * time.Millisecond)
		concurrent.Add(-1)
		grant.Release()
	}
	for range N / 2 {
		go acquire(cA)
		go acquire(cB)
	}
	wg.Wait()

	if got := maxConcurrent.Load(); got != 1 {
		t.Errorf("max concurrent grants across Coords = %d, want 1 (flock-as-serialiser invariant)", got)
	}
}

func TestStalledLiveCreatorAdopterSucceeds(t *testing.T) {
	// The adopter side of Open has an errPartialInit retry path: a
	// creator can be inside the open→flock(LOCK_EX) window when an
	// adopter lands. Real (sub-millisecond) init never exercises this
	// in TestConcurrentOpenRaceWithCrossMmapVisibility, so this test
	// uses SetCreateInitHookForTest to widen the window: the hook
	// closes adopterStart so the adopter can run only AFTER the
	// creator's O_CREATE|O_EXCL has succeeded but BEFORE its
	// LOCK_EX — guaranteeing the adopter lands inside the partial-init
	// window. The 60 ms sleep gives the adopter several retry rounds
	// against errPartialInit. Asserts the adopter converges to a
	// successful Open (no ErrCorrupted, no blocking) and that
	// cross-mmap writes via the creator are visible from the adopter.
	root, base, _ := tmpLock(t)
	uuid := [16]byte{0xAB}

	adopterStart := make(chan struct{})
	SetCreateInitHookForTest(func() {
		select {
		case <-adopterStart:
		default:
			close(adopterStart)
		}
		time.Sleep(60 * time.Millisecond)
	})
	t.Cleanup(func() { SetCreateInitHookForTest(nil) })

	type result struct {
		f   *File
		err error
	}
	creatorDone := make(chan result, 1)
	go func() {
		f, err := Open(OpenParams{Root: root, Base: base, DataUUID: uuid, MaxReaders: 8})
		creatorDone <- result{f, err}
	}()

	// Wait for the creator to enter the injected window.
	select {
	case <-adopterStart:
	case <-time.After(time.Second):
		t.Fatalf("creator never reached the createInitHookForTest hook")
	}

	// Now adopt. The first attempt sees a zero-size or Magic==0 file
	// and surfaces errPartialInit; backoff retries until the creator
	// publishes.
	adopter, err := Open(OpenParams{Root: root, Base: base, DataUUID: uuid, MaxReaders: 8})
	if err != nil {
		t.Fatalf("adopter Open: %v", err)
	}
	defer adopter.Close()

	r := <-creatorDone
	if r.err != nil {
		t.Fatalf("creator Open: %v", r.err)
	}
	defer r.f.Close()

	if got := adopter.UUID(); got != uuid {
		t.Errorf("adopter UUID = %x, want %x", got, uuid)
	}
	if got := r.f.UUID(); got != uuid {
		t.Errorf("creator UUID = %x, want %x", got, uuid)
	}

	// Cross-mmap visibility: a write via creator must be visible from
	// adopter.
	r.f.SetWriterPID(0xBEEF)
	if got := adopter.WriterPID(); got != 0xBEEF {
		t.Errorf("adopter sees WriterPID = %x, want 0xBEEF (no split-brain)", got)
	}
}

// TestReadOnlyCoordSkipsFlockGrant verifies the read-only coord mode
// (CoordOptions.ReadOnly): the flock-grant goroutine is not started so
// AcquireWriter is refused with ErrReadOnlyCoord, while the reader-slot
// path (served without the flock goroutine) and a prompt Close still
// work. A Close that hung — doneCh never closed because run() never
// started — would trip the test's deadline.
func TestReadOnlyCoordSkipsFlockGrant(t *testing.T) {
	root, base, _ := tmpLock(t)
	f, err := Open(OpenParams{
		Root: root, Base: base, DataUUID: [16]byte{0x5A}, MaxReaders: 4,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = f.Close() }()
	c := NewCoord(f, CoordOptions{
		PID:           1,
		ReadOnly:      true,
		RetryInterval: 10 * time.Millisecond,
	})

	// No flock-grant goroutine: AcquireWriter is refused outright
	// rather than blocking forever on an unserved writerCh.
	if _, err := c.AcquireWriter(context.Background()); !errors.Is(err, ErrReadOnlyCoord) {
		t.Errorf("AcquireWriter on read-only coord: got %v, want ErrReadOnlyCoord", err)
	}

	// Reader-slot acquisition (and release) still works.
	idx, err := c.AcquireReader(context.Background(), 7)
	if err != nil {
		t.Fatalf("AcquireReader on read-only coord: %v", err)
	}
	if idx == NoSlot {
		t.Fatal("AcquireReader returned NoSlot")
	}
	c.ReleaseReader(idx)

	// Close must return promptly even though run() never started
	// (doneCh was pre-closed). Guard with a deadline so a regression
	// that waits on an un-closed doneCh fails loudly instead of hanging.
	done := make(chan struct{})
	go func() { _ = c.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("read-only coord Close hung (doneCh never closed?)")
	}
}

package lock

import (
	"context"
	"errors"
	"os"
	"sync"
	"syscall"
	"testing"
	"time"
)

// staleWriterFile seeds a fresh *File's writer-header with the given
// dead-writer state so IsStaleWriter / RecoverStaleWriter can be
// exercised against deterministic input.
func staleWriterFile(t *testing.T, pid, startTime, pidNS, heartbeat uint64) *File {
	t.Helper()
	root, base, _ := tmpLock(t)
	f, err := Open(OpenParams{
		Root: root, Base: base, DataUUID: [16]byte{0xAA, 0xBB}, MaxReaders: 8,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })
	f.SetWriterPID(pid)
	f.SetWriterStartTime(startTime)
	f.SetWriterPIDNamespace(pidNS)
	f.SetWriterHeartbeat(heartbeat)
	return f
}

func TestIsStaleWriterNoWriter(t *testing.T) {
	f := staleWriterFile(t, 0, 0, 0, 0)
	if IsStaleWriter(f, 100, 0, uint64(DefaultStaleTimeout.Nanoseconds())) {
		t.Errorf("IsStaleWriter with WriterPID=0 = true; want false")
	}
}

func TestIsStaleWriterSameNSDead(t *testing.T) {
	// Impossibly-high PID — kill(0x7FFFFFFF, 0) returns ESRCH on
	// every realistic Linux kernel.
	f := staleWriterFile(t, 0x7FFFFFFF, 12345, 42 /*ns*/, 100)
	if !IsStaleWriter(f, 42, 9999, uint64(DefaultStaleTimeout.Nanoseconds())) {
		t.Errorf("IsStaleWriter for dead same-NS writer = false; want true")
	}
}

func TestIsStaleWriterSameNSAliveSameStart(t *testing.T) {
	// Self-PID with our own start time — must classify as alive.
	ownStart, err := ProcessStartTime(os.Getpid())
	if err != nil {
		t.Skipf("ProcessStartTime(self): %v", err)
	}
	ownNS, _ := PIDNamespace()
	if ownNS == 0 {
		t.Skip("PIDNamespace returned 0; same-NS path unexercisable in this environment")
	}
	f := staleWriterFile(t, uint64(os.Getpid()), ownStart, ownNS, 100)
	if IsStaleWriter(f, ownNS, 9999, uint64(DefaultStaleTimeout.Nanoseconds())) {
		t.Errorf("IsStaleWriter for live same-NS writer = true; want false")
	}
}

func TestIsStaleWriterSameNSAliveDifferentStart(t *testing.T) {
	// Self-PID but recorded a DIFFERENT start time — PID-reuse case.
	ownStart, err := ProcessStartTime(os.Getpid())
	if err != nil {
		t.Skipf("ProcessStartTime(self): %v", err)
	}
	ownNS, _ := PIDNamespace()
	if ownNS == 0 {
		t.Skip("PIDNamespace returned 0")
	}
	// Seed start = ownStart ± 1 so it provably differs.
	fakeStart := ownStart + 1
	f := staleWriterFile(t, uint64(os.Getpid()), fakeStart, ownNS, 100)
	if !IsStaleWriter(f, ownNS, 9999, uint64(DefaultStaleTimeout.Nanoseconds())) {
		t.Errorf("IsStaleWriter for live but PID-recycled writer = false; want true")
	}
}

func TestIsStaleWriterCrossNSFreshHeartbeat(t *testing.T) {
	// Different namespaces (1 vs 2) — routes through heartbeat. Fresh
	// heartbeat ⇒ alive.
	now := uint64(100_000_000_000)    // 100 s monotonic
	hb := now - uint64(1_000_000_000) // 1 s ago — fresh vs 10 s timeout
	f := staleWriterFile(t, 12345, 999, 1, hb)
	if IsStaleWriter(f, 2, now, uint64(DefaultStaleTimeout.Nanoseconds())) {
		t.Errorf("IsStaleWriter for cross-NS fresh heartbeat = true; want false")
	}
}

func TestIsStaleWriterCrossNSStaleHeartbeat(t *testing.T) {
	// Different namespaces, heartbeat older than StaleTimeout ⇒ stale.
	now := uint64(100_000_000_000)
	hb := now - uint64(20_000_000_000) // 20 s ago > 10 s timeout
	f := staleWriterFile(t, 12345, 999, 1, hb)
	if !IsStaleWriter(f, 2, now, uint64(DefaultStaleTimeout.Nanoseconds())) {
		t.Errorf("IsStaleWriter for cross-NS stale heartbeat = false; want true")
	}
}

func TestIsStaleWriterEitherNSZero(t *testing.T) {
	// One side has PIDNamespace=0 (e.g., non-Linux peer or /proc
	// missing) — routes through heartbeat regardless. Fresh ⇒ alive,
	// stale ⇒ dead. Verify both branches.
	now := uint64(50_000_000_000)
	timeout := uint64(DefaultStaleTimeout.Nanoseconds())

	t.Run("wNS=0 fresh hb", func(t *testing.T) {
		f := staleWriterFile(t, 1, 1, 0 /*wNS*/, now-1_000_000_000)
		if IsStaleWriter(f, 1 /*ours*/, now, timeout) {
			t.Errorf("classified stale; want alive (heartbeat fresh)")
		}
	})
	t.Run("ours=0 stale hb", func(t *testing.T) {
		f := staleWriterFile(t, 1, 1, 1 /*wNS*/, now-15_000_000_000)
		if !IsStaleWriter(f, 0 /*ours*/, now, timeout) {
			t.Errorf("classified alive; want stale (heartbeat aged out)")
		}
	})
}

func TestIsStaleWriterFutureHeartbeatTreatedFresh(t *testing.T) {
	// Defensive: WriterHeartbeat > now (impossible under
	// CLOCK_BOOTTIME / CLOCK_MONOTONIC on a single host but could
	// surface from clock skew if shared storage were ever
	// supported). Treat as fresh — conservative-safer.
	f := staleWriterFile(t, 1, 1, 0, 9_000_000_000)
	if IsStaleWriter(f, 0, 5_000_000_000, uint64(DefaultStaleTimeout.Nanoseconds())) {
		t.Errorf("future heartbeat classified stale; want fresh (conservative)")
	}
}

func TestRecoverStaleWriterClearsHeader(t *testing.T) {
	f := staleWriterFile(t, 999, 88, 42, 7777)
	RecoverStaleWriter(f, 42)
	if got := f.WriterPID(); got != 0 {
		t.Errorf("post-recover WriterPID = %d, want 0", got)
	}
	if got := f.WriterStartTime(); got != 0 {
		t.Errorf("post-recover WriterStartTime = %d, want 0", got)
	}
	if got := f.WriterPIDNamespace(); got != 0 {
		t.Errorf("post-recover WriterPIDNamespace = %d, want 0", got)
	}
	if got := f.WriterHeartbeat(); got != 0 {
		t.Errorf("post-recover WriterHeartbeat = %d, want 0 (recovery clears all four)", got)
	}
}

func TestRecoverStaleWriterClearsMatchingSlots(t *testing.T) {
	// Seed three reader slots: one matches the dead writer's
	// (PID, namespace, startTime), one matches PID + NS but not
	// startTime (PID-reuse — see TestRecoverStaleWriterSkips-
	// RecycledPIDSlots for the dedicated test), one matches PID
	// but not namespace, one matches neither. Only the first
	// should be cleared.
	const deadPID, deadNS, deadStart = uint64(444), uint64(42), uint64(7)
	f := staleWriterFile(t, deadPID, deadStart, deadNS, 100)

	s0 := f.Slot(0)
	Store64(&s0.TxnID, 0x111)
	Store64(&s0.PID, deadPID)
	Store64(&s0.PIDNamespace, deadNS)
	Store64(&s0.ProcessStartTime, deadStart)
	Store64(&s0.Heartbeat, 5_000)
	Store64(&s0.HintEpoch, 0x222)

	s1 := f.Slot(1)
	Store64(&s1.TxnID, 0x333)
	Store64(&s1.PID, deadPID)
	Store64(&s1.PIDNamespace, deadNS+1) // different namespace
	Store64(&s1.ProcessStartTime, deadStart)
	Store64(&s1.Heartbeat, 6_000)

	s2 := f.Slot(2)
	Store64(&s2.TxnID, 0x555)
	Store64(&s2.PID, 999) // different PID
	Store64(&s2.PIDNamespace, deadNS)
	Store64(&s2.ProcessStartTime, deadStart)

	RecoverStaleWriter(f, deadNS)

	// Slot 0: cleared.
	if got := Load64(&s0.TxnID); got != 0 {
		t.Errorf("slot 0 TxnID = %x, want 0 (matched dead writer)", got)
	}
	if got := Load64(&s0.PID); got != 0 {
		t.Errorf("slot 0 PID = %d, want 0", got)
	}
	if got := Load64(&s0.Heartbeat); got != 0 {
		t.Errorf("slot 0 Heartbeat = %d, want 0", got)
	}
	if got := Load64(&s0.HintEpoch); got != 0 {
		t.Errorf("slot 0 HintEpoch = %x, want 0", got)
	}

	// Slot 1: untouched (different namespace).
	if got := Load64(&s1.TxnID); got != 0x333 {
		t.Errorf("slot 1 TxnID = %x, want 0x333 (different namespace)", got)
	}
	if got := Load64(&s1.PID); got != deadPID {
		t.Errorf("slot 1 PID = %d, want %d", got, deadPID)
	}

	// Slot 2: untouched (different PID).
	if got := Load64(&s2.TxnID); got != 0x555 {
		t.Errorf("slot 2 TxnID = %x, want 0x555 (different PID)", got)
	}
}

func TestRecoverStaleWriterSkipsRecycledPIDSlots(t *testing.T) {
	// PID-reuse safety (cross-process.md §Stale Writer Recovery
	// step 2): a slot matching (PID, PIDNamespace) but with a
	// DIFFERENT ProcessStartTime belongs to a recycled-PID live
	// reader, NOT the dead writer — recovery MUST NOT clear it.
	// Without the ProcessStartTime term, recovery wipes the live
	// reader's snapshot (snapshot loss).
	const deadPID, deadNS, deadStart = uint64(444), uint64(42), uint64(0xAAAA)
	const liveReaderStart = uint64(0xBBBB) // recycled PID, different start
	f := staleWriterFile(t, deadPID, deadStart, deadNS, 100)

	// Slot 0: belongs to the recycled-PID LIVE reader. Same PID
	// and namespace as the dead writer, but different start time.
	s0 := f.Slot(0)
	Store64(&s0.TxnID, 0xCAFE)
	Store64(&s0.PID, deadPID)
	Store64(&s0.PIDNamespace, deadNS)
	Store64(&s0.ProcessStartTime, liveReaderStart)
	Store64(&s0.Heartbeat, 5_000)

	// Slot 1: belongs to the actual dead writer's reader-tx (same
	// PID, same namespace, same start time). MUST be cleared.
	s1 := f.Slot(1)
	Store64(&s1.TxnID, 0xDEAD)
	Store64(&s1.PID, deadPID)
	Store64(&s1.PIDNamespace, deadNS)
	Store64(&s1.ProcessStartTime, deadStart)
	Store64(&s1.Heartbeat, 6_000)

	RecoverStaleWriter(f, deadNS)

	// Slot 0: must NOT be touched (recycled-PID live reader).
	if got := Load64(&s0.TxnID); got != 0xCAFE {
		t.Errorf("slot 0 TxnID = %x, want 0xCAFE (recycled-PID live reader wiped — PID-reuse bug)", got)
	}
	if got := Load64(&s0.PID); got != deadPID {
		t.Errorf("slot 0 PID = %d, want %d (live reader stomped)", got, deadPID)
	}

	// Slot 1: MUST have been cleared (dead writer's actual slot).
	if got := Load64(&s1.TxnID); got != 0 {
		t.Errorf("slot 1 TxnID = %x, want 0 (dead writer's slot not cleared)", got)
	}
	if got := Load64(&s1.PID); got != 0 {
		t.Errorf("slot 1 PID = %d, want 0", got)
	}
}

func TestRecoverStaleWriterSkipsCrossNSSlots(t *testing.T) {
	// Cross-namespace recovery: writer header is in a different
	// namespace than ours. Reader slots are NOT scanned — they're
	// not directly comparable across namespaces.
	const deadPID, deadNS = uint64(444), uint64(1)
	f := staleWriterFile(t, deadPID, 7, deadNS, 100)

	s0 := f.Slot(0)
	Store64(&s0.TxnID, 0x111)
	Store64(&s0.PID, deadPID)
	Store64(&s0.PIDNamespace, deadNS)
	Store64(&s0.Heartbeat, 5_000)

	RecoverStaleWriter(f, 2 /*ourNS*/)

	// Header cleared.
	if got := f.WriterPID(); got != 0 {
		t.Errorf("WriterPID = %d, want 0", got)
	}
	// Slot 0 untouched — cross-namespace skips the scan.
	if got := Load64(&s0.TxnID); got != 0x111 {
		t.Errorf("slot 0 TxnID = %x, want 0x111 (cross-NS recovery skips slot scan)", got)
	}
}

func TestRecoverStaleWriterIdempotentOnCleanHeader(t *testing.T) {
	// Recovery on an already-clean header: no-op.
	root, base, _ := tmpLock(t)
	f, err := Open(OpenParams{Root: root, Base: base, DataUUID: [16]byte{0xCC}, MaxReaders: 4})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer f.Close()
	RecoverStaleWriter(f, 1)
	if got := f.WriterPID(); got != 0 {
		t.Errorf("WriterPID = %d, want 0", got)
	}
}

func TestCoordRecoversStaleHeaderOnGrant(t *testing.T) {
	// End-to-end: a *File with stale writer-header state is
	// "adopted" by a fresh Coord. The first AcquireWriter must
	// clear the stale state (RecoverStaleWriter invoked between
	// LOCK_EX|LOCK_NB success and identity publish) and then
	// publish OUR identity over the cleared slots — including
	// WriterHeartbeat, which recovery clears to 0 and step-3
	// republishes via c.clock().
	root, base, _ := tmpLock(t)
	f, err := Open(OpenParams{
		Root: root, Base: base, DataUUID: [16]byte{0xDE}, MaxReaders: 8,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer f.Close()

	// Seed dead-writer state (impossibly-high PID).
	f.SetWriterPID(0x7FFFFFFF)
	f.SetWriterStartTime(9999)
	f.SetWriterPIDNamespace(123)
	f.SetWriterHeartbeat(0xAAAA)

	// Deterministic clock so the post-publish WriterHeartbeat is
	// exactly the injected value.
	const clockValue uint64 = 0xCAFED00D
	clk := newFakeClock(clockValue)

	c := NewCoord(f, CoordOptions{
		PID:               42,
		ProcessStartTime:  7,
		PIDNamespace:      123,
		RetryInterval:     time.Millisecond,
		HeartbeatInterval: time.Hour, // disable goroutine ticks for determinism
		Clock:             clk.now,
	})
	defer c.Close()

	grant, err := c.AcquireWriter(context.Background())
	if err != nil {
		t.Fatalf("AcquireWriter: %v", err)
	}
	defer grant.Release()

	// Identity must reflect THIS Coord (not the stale state).
	if got := f.WriterPID(); got != 42 {
		t.Errorf("WriterPID = %d, want 42 (recovery should have cleared stale 0x7FFFFFFF before publish)", got)
	}
	if got := f.WriterStartTime(); got != 7 {
		t.Errorf("WriterStartTime = %d, want 7", got)
	}
	if got := f.WriterPIDNamespace(); got != 123 {
		t.Errorf("WriterPIDNamespace = %d, want 123", got)
	}
	if got := f.WriterHeartbeat(); got != clockValue {
		t.Errorf("WriterHeartbeat = 0x%X, want 0x%X (stale 0xAAAA must have been cleared by recovery then republished via step-3)",
			got, clockValue)
	}
}

func TestCoordRecoversStaleReaderSlotsOnGrant(t *testing.T) {
	// Stale writer state + reader slots owned by the dead writer.
	// On grant, recovery clears both.
	root, base, _ := tmpLock(t)
	f, err := Open(OpenParams{
		Root: root, Base: base, DataUUID: [16]byte{0xEE}, MaxReaders: 4,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer f.Close()

	const deadPID, deadNS uint64 = 0x7FFFFFFF, 555
	f.SetWriterPID(deadPID)
	f.SetWriterPIDNamespace(deadNS)

	s := f.Slot(2)
	Store64(&s.TxnID, 0xABCD)
	Store64(&s.PID, deadPID)
	Store64(&s.PIDNamespace, deadNS)
	Store64(&s.Heartbeat, 7777)
	Store64(&s.HintEpoch, 0xDEAD)

	c := NewCoord(f, CoordOptions{
		PID:               1,
		PIDNamespace:      deadNS, // same NS — slot cleanup eligible
		RetryInterval:     time.Millisecond,
		HeartbeatInterval: time.Hour,
	})
	defer c.Close()

	grant, err := c.AcquireWriter(context.Background())
	if err != nil {
		t.Fatalf("AcquireWriter: %v", err)
	}
	defer grant.Release()

	// Slot 2 should have been cleared by recovery.
	if got := Load64(&s.TxnID); got != 0 {
		t.Errorf("slot TxnID = %x, want 0", got)
	}
	if got := Load64(&s.PID); got != 0 {
		t.Errorf("slot PID = %d, want 0", got)
	}
	if got := Load64(&s.Heartbeat); got != 0 {
		t.Errorf("slot Heartbeat = %d, want 0", got)
	}
	if got := Load64(&s.HintEpoch); got != 0 {
		t.Errorf("slot HintEpoch = %x, want 0", got)
	}
}

func TestClearBeforeUnlockOrdering(t *testing.T) {
	// Directly enforces the cross-process.md "clear-before-unlock"
	// clause-explicit invariant: when the flock goroutine releases
	// the write lock, identity fields MUST be cleared BEFORE
	// flock(LOCK_UN). Without this, a peer acquiring LOCK_EX
	// immediately after our LOCK_UN would read our stale WriterPID
	// and run stale-writer-recovery against what is actually a
	// cleanly-released slot — racing the peer's own commit.
	//
	// Verification: install a release hook that fires between the
	// field clear and the LOCK_UN syscall. Inside the hook, two
	// witnesses fire:
	//   (a) read WriterPID via this Coord — must already be 0;
	//   (b) issue flock(LOCK_EX|LOCK_NB) on a witness *File on the
	//       same inode — must EWOULDBLOCK (we still hold LOCK_EX,
	//       even though we've cleared the fields).
	// Together: cleared-and-still-locked == the invariant.
	root, base, _ := tmpLock(t)
	uuid := [16]byte{0xC1, 0xE4}
	f, err := Open(OpenParams{Root: root, Base: base, DataUUID: uuid, MaxReaders: 4})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer f.Close()
	witness, err := Open(OpenParams{Root: root, Base: base, DataUUID: uuid, MaxReaders: 4})
	if err != nil {
		t.Fatalf("Open witness: %v", err)
	}
	defer witness.Close()

	c := NewCoord(f, CoordOptions{
		PID:               7777,
		ProcessStartTime:  8888,
		PIDNamespace:      9999,
		RetryInterval:     time.Millisecond,
		HeartbeatInterval: time.Hour,
	})
	defer c.Close()

	type witnessResult struct {
		pidAtHook       uint64
		startAtHook     uint64
		nsAtHook        uint64
		witnessFlockErr error
	}
	witnessCh := make(chan witnessResult, 1)

	SetReleaseHookForTest(func() {
		// We're inside the flock-goroutine, AFTER the field clear
		// and BEFORE LOCK_UN. Read what a peer would see at this
		// instant.
		pid := f.WriterPID()
		start := f.WriterStartTime()
		ns := f.WriterPIDNamespace()
		// Cross-OFD probe: must EWOULDBLOCK — we still hold LOCK_EX.
		ferr := syscall.Flock(int(witness.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if ferr == nil {
			// Defensive: release if we somehow acquired (would
			// indicate the test setup is wrong, not the invariant).
			_ = syscall.Flock(int(witness.Fd()), syscall.LOCK_UN)
		}
		witnessCh <- witnessResult{pid, start, ns, ferr}
	})
	t.Cleanup(func() { SetReleaseHookForTest(nil) })

	grant, err := c.AcquireWriter(context.Background())
	if err != nil {
		t.Fatalf("AcquireWriter: %v", err)
	}
	// Sanity: fields are published.
	if got := f.WriterPID(); got != 7777 {
		t.Fatalf("WriterPID under grant = %d, want 7777", got)
	}

	// Release triggers the hook.
	grant.Release()

	var w witnessResult
	select {
	case w = <-witnessCh:
	case <-time.After(2 * time.Second):
		t.Fatalf("release hook never fired")
	}

	if w.pidAtHook != 0 {
		t.Errorf("WriterPID at hook = %d, want 0 (clear-before-unlock violated — cleared after LOCK_UN)", w.pidAtHook)
	}
	if w.startAtHook != 0 {
		t.Errorf("WriterStartTime at hook = %d, want 0", w.startAtHook)
	}
	if w.nsAtHook != 0 {
		t.Errorf("WriterPIDNamespace at hook = %d, want 0", w.nsAtHook)
	}
	if !errors.Is(w.witnessFlockErr, syscall.EWOULDBLOCK) {
		t.Errorf("witness flock during hook: got %v, want EWOULDBLOCK (LOCK_EX still held at clear-before-unlock point)", w.witnessFlockErr)
	}
}

func TestReleaseHookConcurrentSetGet(t *testing.T) {
	// SetReleaseHookForTest uses atomic.Pointer; concurrent
	// Set with the goroutine's Load must not race.
	root, base, _ := tmpLock(t)
	f, err := Open(OpenParams{Root: root, Base: base, DataUUID: [16]byte{0x11}, MaxReaders: 4})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer f.Close()
	c := NewCoord(f, CoordOptions{PID: 1, RetryInterval: time.Millisecond, HeartbeatInterval: time.Hour})
	defer c.Close()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for range 200 {
			SetReleaseHookForTest(func() {})
			SetReleaseHookForTest(nil)
		}
	}()
	go func() {
		defer wg.Done()
		for range 50 {
			grant, err := c.AcquireWriter(context.Background())
			if err != nil {
				t.Errorf("AcquireWriter: %v", err)
				return
			}
			grant.Release()
		}
	}()
	wg.Wait()
	SetReleaseHookForTest(nil)
}

// TestPrevLastWriterLiveClassification pins the recovery-commit gate's
// author-liveness classification (durability.md §Recovery step 5)
// against forged pre-acquisition records — the production-common crash
// shape where the lock file PERSISTS with a dead author's record (the
// deleted-lock-file path only exercises the pid==0 fast path).
func TestPrevLastWriterLiveClassification(t *testing.T) {
	ourNS, _ := PIDNamespace()
	now := uint64(1_000_000_000_000)
	timeout := uint64(10_000_000_000) // 10s

	mkCoord := func(prevPID, prevStart, prevNS, prevHB uint64) *Coord {
		c := &Coord{pidNS: ourNS, clock: func() uint64 { return now }}
		c.staleTimeout = time.Duration(timeout)
		c.prevLastWriter.pid = prevPID
		c.prevLastWriter.startTime = prevStart
		c.prevLastWriter.pidNS = prevNS
		c.prevLastWriter.heartbeat = prevHB
		return c
	}

	t.Run("no record is not live", func(t *testing.T) {
		if mkCoord(0, 0, 0, 0).PrevLastWriterLive() {
			t.Error("pid 0 classified live")
		}
	})
	t.Run("same-ns dead pid is not live", func(t *testing.T) {
		// A PID far above pid_max cannot be alive.
		if mkCoord(1<<30, 12345, ourNS, now).PrevLastWriterLive() {
			t.Error("dead pid classified live (kill(0) path)")
		}
	})
	t.Run("same-ns live pid with matching start time is live", func(t *testing.T) {
		pid := uint64(os.Getpid())
		start, err := ProcessStartTime(os.Getpid())
		if err != nil {
			t.Skipf("ProcessStartTime: %v", err)
		}
		if !mkCoord(pid, start, ourNS, 0).PrevLastWriterLive() {
			t.Error("our own live process classified dead")
		}
	})
	t.Run("same-ns recycled pid (start-time mismatch) is not live", func(t *testing.T) {
		pid := uint64(os.Getpid())
		start, err := ProcessStartTime(os.Getpid())
		if err != nil {
			t.Skipf("ProcessStartTime: %v", err)
		}
		if mkCoord(pid, start+999, ourNS, now).PrevLastWriterLive() {
			t.Error("recycled pid classified live")
		}
	})
	t.Run("cross-ns fresh heartbeat is live", func(t *testing.T) {
		if !mkCoord(42, 1, ourNS+1, now-timeout/2).PrevLastWriterLive() {
			t.Error("fresh cross-ns heartbeat classified dead")
		}
	})
	t.Run("cross-ns stale heartbeat is not live", func(t *testing.T) {
		if mkCoord(42, 1, ourNS+1, now-2*timeout).PrevLastWriterLive() {
			t.Error("stale cross-ns heartbeat classified live")
		}
	})
}

package lock

import (
	"context"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeClock is a test-time atomic monotonic-clock source. The value
// is in nanoseconds — same units as the per-platform real clock —
// but tests advance it explicitly to make assertions deterministic.
type fakeClock struct {
	v atomic.Uint64
}

func (f *fakeClock) now() uint64  { return f.v.Load() }
func (f *fakeClock) set(v uint64) { f.v.Store(v) }
func newFakeClock(initial uint64) *fakeClock {
	c := &fakeClock{}
	c.v.Store(initial)
	return c
}

// pollUntil polls cond every 2 ms until it returns true or the
// deadline elapses, returning cond's final result. Used to make
// heartbeat assertions robust against scheduler latency on contended
// CI without depending on a fixed-time sleep landing on a tick.
func pollUntil(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// newHeartbeatCoord opens a fresh *File and Coord with the supplied
// heartbeat interval and clock. The Coord is registered for Close in
// t.Cleanup; the *File is closed AFTER the Coord (Coord.Close
// drains the heartbeat goroutine before *File becomes unmappable).
func newHeartbeatCoord(t *testing.T, hbInterval time.Duration, clock func() uint64) (*Coord, *File) {
	t.Helper()
	root, base, _ := tmpLock(t)
	f, err := Open(OpenParams{
		Root: root, Base: base, DataUUID: [16]byte{0xB0, 0xB1}, MaxReaders: 8,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	c := NewCoord(f, CoordOptions{
		PID:               101,
		ProcessStartTime:  202,
		PIDNamespace:      303,
		RetryInterval:     time.Millisecond,
		HeartbeatInterval: hbInterval,
		Clock:             clock,
	})
	t.Cleanup(func() {
		_ = c.Close()
		_ = f.Close()
	})
	return c, f
}

func TestHeartbeatInitialPublishOnGrant(t *testing.T) {
	// Invariant 4: WriterHeartbeat is published under LOCK_EX at
	// grant time, so a peer reading WriterPID != 0 immediately also
	// sees a fresh non-zero WriterHeartbeat — never the zero-init
	// value that would false-stale across a different-namespace scan.
	clk := newFakeClock(1_000_000_000) // 1s in nanos
	c, f := newHeartbeatCoord(t, time.Hour, clk.now)

	if got := f.WriterHeartbeat(); got != 0 {
		t.Fatalf("pre-grant WriterHeartbeat = %d, want 0", got)
	}

	grant, err := c.AcquireWriter(context.Background())
	if err != nil {
		t.Fatalf("AcquireWriter: %v", err)
	}
	defer grant.Release()

	// At this point AcquireWriter has returned, so the publish step
	// (3) completed before the result was sent. WriterHeartbeat must
	// reflect the clock at publish time — not 0, regardless of
	// whether the heartbeat ticker has yet to fire (interval = 1h).
	if got := f.WriterHeartbeat(); got != 1_000_000_000 {
		t.Errorf("initial WriterHeartbeat = %d, want 1_000_000_000", got)
	}
}

func TestHeartbeatWriterRefreshesWhileHolding(t *testing.T) {
	// Invariant 1: while holding the lock, WriterHeartbeat is
	// refreshed each tick. Confirm the value advances after a few
	// ticks.
	clk := newFakeClock(0)
	c, f := newHeartbeatCoord(t, 5*time.Millisecond, clk.now)

	grant, err := c.AcquireWriter(context.Background())
	if err != nil {
		t.Fatalf("AcquireWriter: %v", err)
	}
	defer grant.Release()

	first := f.WriterHeartbeat()
	clk.set(100_000_000) // advance fake clock to 100ms
	// Give the heartbeat goroutine at least 2 ticks to observe the
	// advanced clock value.
	time.Sleep(50 * time.Millisecond)

	second := f.WriterHeartbeat()
	if second <= first {
		t.Errorf("WriterHeartbeat did not advance: first=%d second=%d", first, second)
	}
	if second != 100_000_000 {
		t.Errorf("WriterHeartbeat after advance = %d, want 100_000_000", second)
	}
}

func TestHeartbeatNoWriteOffHold(t *testing.T) {
	// Invariant (cross-process.md §Invariants): WriterHeartbeat is
	// written only by the lock-holding flock goroutine. The general
	// heartbeat goroutine MUST NOT write WriterHeartbeat at all —
	// doing so would stomp the value of whichever process actually
	// holds the lock.
	clk := newFakeClock(0)
	_, f := newHeartbeatCoord(t, 2*time.Millisecond, clk.now)

	// Seed WriterHeartbeat to a known sentinel via direct setter
	// (simulating "another process holds the lock and has published
	// its heartbeat").
	const sentinel = 0xFEEDFACE
	f.SetWriterHeartbeat(sentinel)
	clk.set(123_000_000)

	// Run the heartbeat goroutine for several ticks. We never call
	// AcquireWriter, so the flock goroutine never enters its hold
	// loop; the heartbeat goroutine, the only other goroutine
	// ticking, never writes WriterHeartbeat.
	time.Sleep(30 * time.Millisecond)

	if got := f.WriterHeartbeat(); got != sentinel {
		t.Errorf("WriterHeartbeat = 0x%X, want 0x%X (heartbeat stomped a non-held writer)",
			got, sentinel)
	}
}

func TestHeartbeatStopsAfterRelease(t *testing.T) {
	// After grant.Release the flock goroutine leaves its step-4 hold
	// loop and stops its ticker, so WriterHeartbeat is never written
	// again (the heartbeat goroutine never writes it at all). There is
	// no post-unlock stomp window — the refresh lives in the
	// lock-holding goroutine, not behind a cross-goroutine flag.
	//
	// Robustness: instead of asserting a value at a fixed-time
	// snapshot (which races scheduler latency on contended CI), we
	// settle past any final under-lock refresh (the clock is held
	// constant across that window so it is a value-preserving no-op),
	// snapshot, then assert the value never advances once the clock
	// jumps — proving nothing refreshes WriterHeartbeat off-hold.
	clk := newFakeClock(1_000)
	c, f := newHeartbeatCoord(t, 5*time.Millisecond, clk.now)

	grant, err := c.AcquireWriter(context.Background())
	if err != nil {
		t.Fatalf("AcquireWriter: %v", err)
	}
	grant.Release()

	// Settle: one full tick interval lets the flock goroutine exit its
	// hold loop (any final under-lock refresh lands at the still-held
	// clock value).
	time.Sleep(20 * time.Millisecond)
	settled := f.WriterHeartbeat()

	// Advance clock by a large delta and poll: across the next 200 ms
	// (~40 ticks) WriterHeartbeat MUST NOT change. The flock goroutine
	// has left its hold loop and the heartbeat goroutine never writes
	// WriterHeartbeat, so nothing refreshes it once we are off-hold.
	clk.set(10_000_000)
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		if got := f.WriterHeartbeat(); got != settled {
			t.Errorf("WriterHeartbeat advanced after Release: settled=%d now=%d", settled, got)
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestHeartbeatWriterRefreshConfinedToLockHolder(t *testing.T) {
	// The relocation guarantee (cross-process.md §Invariants):
	// WriterHeartbeat is refreshed ONLY by the flock goroutine while
	// it holds LOCK_EX (its step-4 hold loop), never by the general
	// heartbeat goroutine. Prove the division of labor by contrast —
	// after the writer releases, the heartbeat goroutine is
	// demonstrably still alive and ticking (it keeps refreshing a
	// registered reader slot), yet WriterHeartbeat is frozen because
	// the only goroutine that ever wrote it (the flock goroutine) has
	// left its hold window. A regression that re-adds a WriterHeartbeat
	// write to the heartbeat goroutine fails the final assertion.
	clk := newFakeClock(1_000)
	c, f := newHeartbeatCoord(t, 5*time.Millisecond, clk.now)

	// A live reader slot witnesses that the heartbeat goroutine keeps
	// ticking across the whole test.
	c.RegisterReaderSlot(0)

	grant, err := c.AcquireWriter(context.Background())
	if err != nil {
		t.Fatalf("AcquireWriter: %v", err)
	}

	// While holding: advance the clock and wait for both the writer
	// heartbeat (refreshed by the flock goroutine) and the reader slot
	// (refreshed by the heartbeat goroutine) to reach the new value.
	clk.set(2_000_000)
	if !pollUntil(200*time.Millisecond, func() bool {
		return f.WriterHeartbeat() == 2_000_000 && Load64(&f.Slot(0).Heartbeat) == 2_000_000
	}) {
		t.Fatalf("while holding: writer=%d slot=%d, want both 2_000_000",
			f.WriterHeartbeat(), Load64(&f.Slot(0).Heartbeat))
	}

	grant.Release()
	// Settle: let the flock goroutine leave its hold loop. The clock
	// is still 2_000_000, so any final under-lock refresh is a no-op
	// value-wise.
	time.Sleep(20 * time.Millisecond)
	frozenWriter := f.WriterHeartbeat()

	// After release, advance the clock. The reader slot MUST keep
	// advancing (heartbeat goroutine alive and ticking) — while
	// WriterHeartbeat MUST stay frozen (no goroutine writes it once
	// the hold window ends).
	clk.set(9_000_000)
	if !pollUntil(200*time.Millisecond, func() bool {
		return Load64(&f.Slot(0).Heartbeat) == 9_000_000
	}) {
		t.Fatalf("reader slot did not advance after release: got %d, want 9_000_000 "+
			"(heartbeat goroutine should still be ticking)", Load64(&f.Slot(0).Heartbeat))
	}
	if got := f.WriterHeartbeat(); got != frozenWriter {
		t.Errorf("WriterHeartbeat advanced after release while not held: frozen=%d now=%d",
			frozenWriter, got)
	}
}

func TestHeartbeatActiveSlotsRefresh(t *testing.T) {
	// Register two reader-slot indices; verify the heartbeat goroutine
	// writes Heartbeat to both. Unregister one; verify it stops.
	clk := newFakeClock(0)
	c, f := newHeartbeatCoord(t, 3*time.Millisecond, clk.now)

	c.RegisterReaderSlot(0)
	c.RegisterReaderSlot(3)

	clk.set(5_000_000)
	time.Sleep(20 * time.Millisecond)

	if got := Load64(&f.Slot(0).Heartbeat); got != 5_000_000 {
		t.Errorf("slot 0 Heartbeat = %d, want 5_000_000", got)
	}
	if got := Load64(&f.Slot(3).Heartbeat); got != 5_000_000 {
		t.Errorf("slot 3 Heartbeat = %d, want 5_000_000", got)
	}
	if got := Load64(&f.Slot(1).Heartbeat); got != 0 {
		t.Errorf("slot 1 Heartbeat = %d, want 0 (not registered)", got)
	}

	// Unregister slot 0; advance clock; only slot 3 should advance.
	c.UnregisterReaderSlot(0)
	clk.set(9_000_000)
	time.Sleep(20 * time.Millisecond)

	if got := Load64(&f.Slot(0).Heartbeat); got != 5_000_000 {
		t.Errorf("slot 0 Heartbeat after Unregister = %d, want frozen at 5_000_000", got)
	}
	if got := Load64(&f.Slot(3).Heartbeat); got != 9_000_000 {
		t.Errorf("slot 3 Heartbeat = %d, want 9_000_000", got)
	}
}

func TestHeartbeatUnregisterUnknownIsNoop(t *testing.T) {
	// Idempotent unregister: removing an absent index must not panic
	// and must not corrupt the active list.
	clk := newFakeClock(0)
	c, f := newHeartbeatCoord(t, time.Hour, clk.now)

	c.RegisterReaderSlot(2)
	c.UnregisterReaderSlot(0) // not registered — no-op
	c.UnregisterReaderSlot(2)

	// After both unregisters, no slot should receive a heartbeat.
	clk.set(1_000)
	time.Sleep(5 * time.Millisecond)
	for i := range uint32(8) {
		if got := Load64(&f.Slot(i).Heartbeat); got != 0 {
			t.Errorf("slot %d Heartbeat = %d, want 0 after all unregistered", i, got)
		}
	}
}

func TestHeartbeatCloseWaitsForGoroutine(t *testing.T) {
	// Invariant 2: Close blocks until the heartbeat goroutine exits.
	// We can't directly observe the goroutine count, but we can pin
	// that Close serialises with a final write by counting writes:
	// the slot's Heartbeat field must be stable across multiple
	// Reads after Close returns.
	clk := newFakeClock(7)
	root, base, _ := tmpLock(t)
	f, err := Open(OpenParams{
		Root: root, Base: base, DataUUID: [16]byte{0xC1, 0x05}, MaxReaders: 4,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer f.Close()
	c := NewCoord(f, CoordOptions{
		PID:               1,
		HeartbeatInterval: time.Millisecond,
		Clock:             clk.now,
	})

	c.RegisterReaderSlot(0)
	time.Sleep(10 * time.Millisecond)

	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// After Close, advancing the fake clock + waiting must NOT
	// affect the slot — no goroutine is still ticking.
	frozen := Load64(&f.Slot(0).Heartbeat)
	clk.set(999_999_999)
	time.Sleep(20 * time.Millisecond)
	if got := Load64(&f.Slot(0).Heartbeat); got != frozen {
		t.Errorf("slot Heartbeat changed after Close: was %d, now %d", frozen, got)
	}
}

func TestHeartbeatConcurrentRegisterUnregister(t *testing.T) {
	// Pin the activeSlotsMu protection: N goroutines hammer
	// Register/Unregister while the heartbeat goroutine ticks. Race
	// detector + final-state consistency confirm no torn list.
	clk := newFakeClock(0)
	c, _ := newHeartbeatCoord(t, time.Millisecond, clk.now)

	const N = 4
	const Iter = 200
	var wg sync.WaitGroup
	wg.Add(N)
	for i := range uint32(N) {
		go func() {
			defer wg.Done()
			for range Iter {
				c.RegisterReaderSlot(i)
				c.UnregisterReaderSlot(i)
			}
		}()
	}
	wg.Wait()

	// All slots should be unregistered (each goroutine ends on
	// Unregister); the internal list must be empty.
	c.activeSlotsMu.Lock()
	got := slices.Clone(c.activeSlots)
	c.activeSlotsMu.Unlock()
	if len(got) != 0 {
		t.Errorf("activeSlots after Register/Unregister storm = %v, want empty", got)
	}
}

func TestHeartbeatDefaultClock(t *testing.T) {
	// Nil Clock falls back to nowMonotonic — sanity check that the
	// per-platform default is wired through and produces a non-zero
	// monotonic value.
	root, base, _ := tmpLock(t)
	f, err := Open(OpenParams{
		Root: root, Base: base, DataUUID: [16]byte{0xD0}, MaxReaders: 4,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	c := NewCoord(f, CoordOptions{
		PID:               1,
		HeartbeatInterval: 5 * time.Millisecond,
		// Clock: nil → defaults to nowMonotonic
	})
	t.Cleanup(func() {
		_ = c.Close()
		_ = f.Close()
	})

	c.RegisterReaderSlot(0)
	time.Sleep(20 * time.Millisecond)
	if got := Load64(&f.Slot(0).Heartbeat); got == 0 {
		t.Errorf("default clock produced 0 heartbeat")
	}
}

// TestHeartbeatRefreshesLastWriterForHandleLifetime pins the idle-
// author liveness signal (durability.md §Recovery step 5): after a
// grant RELEASE, the author handle's heartbeat goroutine keeps
// LastWriterHeartbeat fresh — that persistence past release is what
// lets the recovery-commit gate see an idle live author. Also pins
// the ownership guard: once a different process's identity occupies
// the record, this handle stops refreshing it.
func TestHeartbeatRefreshesLastWriterForHandleLifetime(t *testing.T) {
	clk := newFakeClock(1_000_000_000)
	c, f := newHeartbeatCoord(t, 5*time.Millisecond, clk.now)

	g, err := c.AcquireWriter(context.Background())
	if err != nil {
		t.Fatalf("AcquireWriter: %v", err)
	}
	g.Release()
	// Release is served by the flock goroutine; the header clear is
	// asynchronous from the caller's perspective.
	if !pollUntil(2*time.Second, func() bool { return f.WriterPID() == 0 }) {
		t.Fatalf("WriterPID = %d after release, want 0", f.WriterPID())
	}
	if got := f.LastWriterPID(); got != 101 {
		t.Fatalf("LastWriterPID = %d after release, want 101 (persists)", got)
	}

	// Advance the clock; the heartbeat goroutine must refresh the
	// released record on a subsequent tick.
	clk.set(2_000_000_000)
	if !pollUntil(2*time.Second, func() bool {
		return f.LastWriterHeartbeat() == 2_000_000_000
	}) {
		t.Fatalf("LastWriterHeartbeat = %d, want refreshed to 2000000000 after release", f.LastWriterHeartbeat())
	}

	// A peer re-stamps the record; our refresher must stand down.
	f.SetLastWriterPID(999)
	f.SetLastWriterPIDNamespace(303)
	f.SetLastWriterHeartbeat(2_000_000_000)
	clk.set(3_000_000_000)
	time.Sleep(30 * time.Millisecond) // several ticks
	if got := f.LastWriterHeartbeat(); got != 2_000_000_000 {
		t.Errorf("LastWriterHeartbeat = %d after peer re-stamp, want 2000000000 (no refresh of a peer's record)", got)
	}
}

package lock

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Notification region (cross-process.md §Lock File Layout,
// notification region): opaque monotonic version words — global bump
// + keyspace stamps on publish, CAS-max seeding, futex/poll waits
// with wake-through-a-peer-mapping as the cross-process property.

func notifyFixture(t *testing.T) *File {
	t.Helper()
	root, base, _ := tmpLock(t)
	f, err := Open(OpenParams{Root: root, Base: base, DataUUID: [16]byte{1}, MaxReaders: 8})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

func TestKeyspaceNotifySlotRange(t *testing.T) {
	names := []string{"", "a", "users", "sessions", "a-rather-long-keyspace-name", "\x00\xff"}
	for _, n := range names {
		s := KeyspaceNotifySlot(n)
		if s < 1 || s > NotifyKeyspaceSlots {
			t.Errorf("KeyspaceNotifySlot(%q) = %d, want in [1, %d]", n, s, NotifyKeyspaceSlots)
		}
		if s2 := KeyspaceNotifySlot(n); s2 != s {
			t.Errorf("KeyspaceNotifySlot(%q) not deterministic: %d then %d", n, s, s2)
		}
	}
}

func TestSeedNotifyVersionCASMax(t *testing.T) {
	f := notifyFixture(t)
	f.SeedNotifyVersion(10)
	if v := f.NotifyVersion(NotifyGlobalSlot); v != 10 {
		t.Fatalf("after seed 10: %d", v)
	}
	// A lower seed never regresses the word (monotonicity invariant).
	f.SeedNotifyVersion(5)
	if v := f.NotifyVersion(NotifyGlobalSlot); v != 10 {
		t.Fatalf("seed 5 regressed the word: %d, want 10", v)
	}
	f.SeedNotifyVersion(20)
	if v := f.NotifyVersion(NotifyGlobalSlot); v != 20 {
		t.Fatalf("after seed 20: %d", v)
	}
}

func TestPublishCommitBumpsAndStamps(t *testing.T) {
	f := notifyFixture(t)
	f.SeedNotifyVersion(100)
	got := f.PublishCommit([]uint32{3, 7})
	if got != 101 {
		t.Fatalf("PublishCommit = %d, want 101", got)
	}
	if v := f.NotifyVersion(NotifyGlobalSlot); v != 101 {
		t.Errorf("global word = %d, want 101", v)
	}
	// Touched keyspace words carry the GLOBAL value (mutual
	// comparability of all versions), untouched words are unchanged.
	for slot := uint32(1); slot < NotifySlotCount; slot++ {
		want := uint64(0)
		if slot == 3 || slot == 7 {
			want = 101
		}
		if v := f.NotifyVersion(slot); v != want {
			t.Errorf("slot %d = %d, want %d", slot, v, want)
		}
	}
	// A second publish touching one of them advances it again.
	if got := f.PublishCommit([]uint32{7}); got != 102 {
		t.Fatalf("second PublishCommit = %d, want 102", got)
	}
	if v := f.NotifyVersion(3); v != 101 {
		t.Errorf("slot 3 after unrelated publish = %d, want 101", v)
	}
	if v := f.NotifyVersion(7); v != 102 {
		t.Errorf("slot 7 = %d, want 102", v)
	}
}

func TestPublishCommitRejectsBadSlots(t *testing.T) {
	f := notifyFixture(t)
	for _, bad := range []uint32{NotifyGlobalSlot, NotifySlotCount, NotifySlotCount + 5} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("PublishCommit accepted slot %d", bad)
				}
			}()
			f.PublishCommit([]uint32{bad})
		}()
	}
}

func TestWaitNotifyImmediateWhenAlreadyPast(t *testing.T) {
	f := notifyFixture(t)
	f.SeedNotifyVersion(50)
	v, err := f.WaitNotify(context.Background(), NotifyGlobalSlot, 49, nil)
	if err != nil || v != 50 {
		t.Fatalf("WaitNotify = (%d, %v), want (50, nil)", v, err)
	}
}

// TestWaitNotifyWakesAcrossMappings pins the cross-process property:
// a waiter blocked through one mapping of the lock file is woken by
// a publish through a DIFFERENT mapping of the same file (two *File
// handles model two processes; the futex/poll operates on the shared
// page, not the handle).
func TestWaitNotifyWakesAcrossMappings(t *testing.T) {
	root, base, _ := tmpLock(t)
	uuid := [16]byte{2}
	a, err := Open(OpenParams{Root: root, Base: base, DataUUID: uuid, MaxReaders: 8})
	if err != nil {
		t.Fatalf("Open a: %v", err)
	}
	defer a.Close()
	b, err := Open(OpenParams{Root: root, Base: base, DataUUID: uuid, MaxReaders: 8})
	if err != nil {
		t.Fatalf("Open b: %v", err)
	}
	defer b.Close()

	slot := KeyspaceNotifySlot("k")
	from := a.NotifyVersion(slot)
	type res struct {
		v   uint64
		err error
	}
	ch := make(chan res, 1)
	go func() {
		v, err := a.WaitNotify(context.Background(), slot, from, nil)
		ch <- res{v, err}
	}()
	// Give the waiter time to actually block (not load-bearing for
	// correctness — a publish-before-wait is caught by the loop's
	// first read — but it makes the test exercise the wake path).
	time.Sleep(20 * time.Millisecond)
	want := b.PublishCommit([]uint32{slot})
	select {
	case r := <-ch:
		if r.err != nil || r.v != want {
			t.Fatalf("WaitNotify = (%d, %v), want (%d, nil)", r.v, r.err, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waiter not woken by peer-mapping publish within 5s")
	}
}

func TestWaitNotifyContextCancel(t *testing.T) {
	f := notifyFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan error, 1)
	go func() {
		_, err := f.WaitNotify(ctx, NotifyGlobalSlot, 1<<60, nil)
		ch <- err
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-ch:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("WaitNotify after cancel = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancellation did not unblock the waiter within 5s")
	}
}

func TestWaitNotifyStopped(t *testing.T) {
	f := notifyFixture(t)
	_, err := f.WaitNotify(context.Background(), NotifyGlobalSlot, 1<<60, func() bool { return true })
	if !errors.Is(err, ErrNotifyStopped) {
		t.Fatalf("WaitNotify with immediate stop = %v, want ErrNotifyStopped", err)
	}
}

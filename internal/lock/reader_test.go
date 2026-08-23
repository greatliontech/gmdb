package lock

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/greatliontech/gmdb/internal/flock"
)

// openTestFile mints an *os.Root over a fresh temp dir, opens a lock
// file with the given MaxReaders, and registers cleanup. Used by the
// reader-slot tests that want direct *File access without going
// through a Coord.
func openTestFile(t *testing.T, maxReaders uint32) *File {
	t.Helper()
	root, base, _ := tmpLock(t)
	f, err := Open(OpenParams{
		Root: root, Base: base, DataUUID: [16]byte{0xAB, 0xCD, 0xEF},
		MaxReaders: maxReaders,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

// openPeerFile opens a SECOND *File over the same lock file — a
// distinct handle whose hold/probe descriptions are distinct open
// file descriptions, so its locks conflict with the first handle's
// exactly as a separate process's would.
func openPeerFile(t *testing.T, maxReaders uint32) (*File, *File) {
	t.Helper()
	root, base, _ := tmpLock(t)
	params := OpenParams{
		Root: root, Base: base, DataUUID: [16]byte{0xAB, 0xCD, 0xEF},
		MaxReaders: maxReaders,
	}
	a, err := Open(params)
	if err != nil {
		t.Fatalf("Open a: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })
	b, err := Open(params)
	if err != nil {
		t.Fatalf("Open b: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })
	return a, b
}

func TestAcquireReaderSlotBasic(t *testing.T) {
	// Sanity: acquire returns a valid index, TxnID and the
	// diagnostic PID are stored, the reserved fields are zeroed,
	// and the slot's lock is HELD (a peer handle's probe fails).
	a, b := openPeerFile(t, 4)
	idx, err := a.AcquireReaderSlot(0, 42, 7777)
	if err != nil {
		t.Fatalf("AcquireReaderSlot: %v", err)
	}
	if idx >= 4 {
		t.Fatalf("slot index %d out of range", idx)
	}
	slot := a.Slot(idx)
	if got := Load64(&slot.TxnID); got != 42 {
		t.Errorf("TxnID = %d, want 42", got)
	}
	if got := Load64(&slot.PID); got != 7777 {
		t.Errorf("PID = %d, want 7777", got)
	}
	for i, r := range []*uint64{&slot.Reserved1, &slot.Reserved2, &slot.Reserved3, &slot.Reserved4, &slot.Reserved5} {
		if got := Load64(r); got != 0 {
			t.Errorf("Reserved%d = %d, want 0", i+1, got)
		}
	}
	if _, err := b.locks.tryProbe(idx); !errors.Is(err, errSlotBusy) {
		t.Fatalf("peer probe of a held slot: %v, want errSlotBusy", err)
	}
}

func TestAcquireReaderSlotFull(t *testing.T) {
	f := openTestFile(t, 2)
	a, err := f.AcquireReaderSlot(0, 1, 1)
	if err != nil {
		t.Fatalf("acquire 1: %v", err)
	}
	b, err := f.AcquireReaderSlot(0, 2, 1)
	if err != nil {
		t.Fatalf("acquire 2: %v", err)
	}
	if a == b {
		t.Errorf("acquire collided on idx %d", a)
	}
	if _, err := f.AcquireReaderSlot(0, 3, 1); !errors.Is(err, ErrReadersFull) {
		t.Errorf("acquire 3: got %v, want ErrReadersFull", err)
	}
}

func TestAcquireReaderSlotHintSeeds(t *testing.T) {
	// hint=k seeds the scan to start at slot k (occupied slot 0 is
	// genuinely HELD by a peer handle so pass 2 cannot reclaim it).
	a, b := openPeerFile(t, 4)
	if _, err := b.AcquireReaderSlot(0, 99, 1); err != nil {
		t.Fatalf("peer occupy: %v", err)
	}
	idx, err := a.AcquireReaderSlot(2, 1, 1)
	if err != nil {
		t.Fatalf("AcquireReaderSlot hint=2: %v", err)
	}
	if idx != 2 {
		t.Errorf("hint=2 landed on slot %d, want 2", idx)
	}
}

func TestCoordAcquireReaderHintAdvances(t *testing.T) {
	// Coord.AcquireReader stores the just-allocated slot index back
	// into c.readerSlotHint so the next scan starts fresh-ish.
	c, _ := newTestCoord(t, 10*time.Millisecond)
	ctx := context.Background()
	idx, err := c.AcquireReader(ctx, 1)
	if err != nil {
		t.Fatalf("AcquireReader: %v", err)
	}
	if got := c.readerSlotHint.Load(); got != idx {
		t.Errorf("post-Acquire hint = %d, want %d (idx)", got, idx)
	}
	c.ReleaseReader(idx)
}

func TestAcquireReaderSlotTxnIDZeroPanics(t *testing.T) {
	// Documented precondition: txnID=0 collides with the per-slot
	// "free" sentinel. Surface as panic so masking is impossible.
	f := openTestFile(t, 1)
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic on txnID=0")
		}
	}()
	_, _ = f.AcquireReaderSlot(0, 0, 1)
}

func TestReleaseFreesSlotForNextAcquirer(t *testing.T) {
	// Release zeroes content BEFORE dropping the lock, and a peer
	// handle can then acquire the same slot for real.
	a, b := openPeerFile(t, 1)
	idx, err := a.AcquireReaderSlot(0, 7, 1)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if _, err := b.AcquireReaderSlot(0, 8, 2); !errors.Is(err, ErrReadersFull) {
		t.Fatalf("peer acquire of held single slot: %v, want ErrReadersFull", err)
	}
	a.ReleaseReaderSlot(idx)
	if got := Load64(&a.Slot(idx).TxnID); got != 0 {
		t.Fatalf("released TxnID = %d, want 0", got)
	}
	idx2, err := b.AcquireReaderSlot(0, 8, 2)
	if err != nil {
		t.Fatalf("peer acquire after release: %v", err)
	}
	if got := Load64(&b.Slot(idx2).TxnID); got != 8 {
		t.Fatalf("peer TxnID = %d, want 8", got)
	}
}

func TestStaleSlotReclaimedInlineOnFullTable(t *testing.T) {
	// Pass 2 of the acquire scan try-locks every slot: a dead
	// owner's residue (nonzero TxnID, unheld lock) is taken over in
	// place — the inline form of stale reclamation.
	f := openTestFile(t, 1)
	// Dead residue: raw store with no lock held anywhere.
	Store64(&f.Slot(0).TxnID, 99)
	idx, err := f.AcquireReaderSlot(0, 5, 1)
	if err != nil {
		t.Fatalf("acquire over dead residue: %v", err)
	}
	if idx != 0 {
		t.Fatalf("landed on %d, want 0", idx)
	}
	if got := Load64(&f.Slot(0).TxnID); got != 5 {
		t.Fatalf("TxnID = %d, want 5 (residue overwritten)", got)
	}
}

func TestOwnHeldSlotNeverReclaimed(t *testing.T) {
	// Pass 2 must skip THIS handle's own held slots structurally: a
	// try-lock through the same hold description would "succeed"
	// against our own live reader (same-description caveat).
	f := openTestFile(t, 1)
	if _, err := f.AcquireReaderSlot(0, 7, 1); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if _, err := f.AcquireReaderSlot(0, 8, 1); !errors.Is(err, ErrReadersFull) {
		t.Fatalf("second acquire stole own held slot: %v, want ErrReadersFull", err)
	}
	if got := Load64(&f.Slot(0).TxnID); got != 7 {
		t.Fatalf("own slot TxnID = %d, want 7 (untouched)", got)
	}
}

func TestOldestReaderTxnIDEmpty(t *testing.T) {
	f := openTestFile(t, 4)
	if got := f.OldestReaderTxnID(); got != NoReaderTxnID {
		t.Errorf("empty table oldest = %d, want NoReaderTxnID", got)
	}
}

func TestOldestReaderTxnIDMinOfMany(t *testing.T) {
	f := openTestFile(t, 4)
	for _, txn := range []uint64{30, 10, 20} {
		if _, err := f.AcquireReaderSlot(0, txn, 1); err != nil {
			t.Fatalf("acquire %d: %v", txn, err)
		}
	}
	if got := f.OldestReaderTxnID(); got != 10 {
		t.Errorf("oldest = %d, want 10", got)
	}
}

func TestOldestReaderTxnIDStalePinsUntilReap(t *testing.T) {
	// The bound scan is a pure memory read: a dead owner's residue
	// conservatively pins the bound; the probe-based reap clears it
	// and the bound recovers.
	f := openTestFile(t, 4)
	if _, err := f.AcquireReaderSlot(0, 30, 1); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	Store64(&f.Slot(3).TxnID, 5) // dead residue, unheld
	if got := f.OldestReaderTxnID(); got != 5 {
		t.Fatalf("oldest with stale = %d, want 5 (conservative pin)", got)
	}
	if cleared, _ := f.ReapStaleReaderSlots(); cleared != 1 {
		t.Fatalf("reap cleared %d, want 1", cleared)
	}
	if got := f.OldestReaderTxnID(); got != 30 {
		t.Fatalf("oldest after reap = %d, want 30", got)
	}
}

func TestReapNeverEvictsLiveReader(t *testing.T) {
	// The verdict and the clear are one act under the slot's lock: a
	// peer's reap probes the held lock, fails, and touches nothing —
	// the heartbeat era's guarded-clear races are unrepresentable.
	a, b := openPeerFile(t, 2)
	idx, err := a.AcquireReaderSlot(0, 7, 1)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	Store64(&b.Slot(1).TxnID, 9) // dead residue beside the live slot
	if cleared, _ := b.ReapStaleReaderSlots(); cleared != 1 {
		t.Fatalf("peer reap cleared %d, want 1 (the residue only)", cleared)
	}
	if got := Load64(&a.Slot(idx).TxnID); got != 7 {
		t.Fatalf("live slot TxnID = %d, want 7 (never evicted)", got)
	}
	a.ReleaseReaderSlot(idx)
}

func TestReapSkipsOwnHeldSlots(t *testing.T) {
	f := openTestFile(t, 2)
	idx, err := f.AcquireReaderSlot(0, 7, 1)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if cleared, _ := f.ReapStaleReaderSlots(); cleared != 0 {
		t.Fatalf("reap cleared %d of its own handle's slots, want 0", cleared)
	}
	if got := Load64(&f.Slot(idx).TxnID); got != 7 {
		t.Fatalf("own slot TxnID = %d, want 7", got)
	}
}

func TestRaiseReaderSlotTxnID(t *testing.T) {
	// The restabilization raise is an owner-only overwrite under the
	// held lock.
	f := openTestFile(t, 1)
	idx, err := f.AcquireReaderSlot(0, 7, 1)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	f.RaiseReaderSlotTxnID(idx, 9)
	if got := Load64(&f.Slot(idx).TxnID); got != 9 {
		t.Fatalf("raised TxnID = %d, want 9", got)
	}
}

func TestSlotLockSurvivesOwnerHandleClose(t *testing.T) {
	// A read transaction may outlive DB.Close: the slot's lock must
	// stay held until the transaction's own release — the hold
	// description rides the mapping's refcount
	// (cross-process.md §Reader Table, descriptions outlive Close).
	a, b := openPeerFile(t, 1)
	idx, err := a.AcquireReaderSlot(0, 7, 1)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	a.Ref()       // the outstanding transaction's lifetime reference
	_ = a.Close() // the owning handle's drop — NOT the last
	if cleared, _ := b.ReapStaleReaderSlots(); cleared != 0 {
		t.Fatalf("peer reaped %d slots under a post-Close live transaction, want 0", cleared)
	}
	a.ReleaseReaderSlot(idx)
	_ = a.Close() // the transaction's drop — final
	if cleared, _ := b.ReapStaleReaderSlots(); cleared != 0 {
		t.Fatalf("reap after clean release cleared %d, want 0 (slot was zeroed)", cleared)
	}
}

func TestCoordAcquireReleaseReader(t *testing.T) {
	c, f := newTestCoord(t, 10*time.Millisecond)
	ctx := context.Background()
	idx, err := c.AcquireReader(ctx, 42)
	if err != nil {
		t.Fatalf("AcquireReader: %v", err)
	}
	if got := Load64(&f.Slot(idx).TxnID); got != 42 {
		t.Errorf("TxnID = %d, want 42", got)
	}
	if got := c.ActiveReaderSlots(); got != 1 {
		t.Errorf("ActiveReaderSlots = %d, want 1", got)
	}
	c.ReleaseReader(idx)
	if got := c.ActiveReaderSlots(); got != 0 {
		t.Errorf("ActiveReaderSlots after release = %d, want 0", got)
	}
	if got := Load64(&f.Slot(idx).TxnID); got != 0 {
		t.Errorf("TxnID after release = %d, want 0", got)
	}
}

func TestCoordAcquireReaderRespectsCtxCancel(t *testing.T) {
	c, _ := newTestCoord(t, 10*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.AcquireReader(ctx, 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("AcquireReader on cancelled ctx: %v", err)
	}
}

func TestCoordAcquireReaderFullNoDeadline(t *testing.T) {
	// A full table with an undeadlined ctx surfaces ErrReadersFull
	// immediately (transactions.md §Read Transaction step 2).
	c, f := newTestCoord(t, 10*time.Millisecond)
	max := f.MaxReaders()
	for i := uint32(0); i < max; i++ {
		if _, err := c.AcquireReader(context.Background(), 1); err != nil {
			t.Fatalf("fill %d: %v", i, err)
		}
	}
	if _, err := c.AcquireReader(context.Background(), 1); !errors.Is(err, ErrReadersFull) {
		t.Fatalf("full, no deadline: %v, want ErrReadersFull", err)
	}
}

func TestCoordAcquireReaderFullWithDeadline(t *testing.T) {
	// With a deadline, the acquirer retries until a slot frees.
	c, f := newTestCoord(t, 10*time.Millisecond)
	max := f.MaxReaders()
	slots := make([]uint32, 0, max)
	for i := uint32(0); i < max; i++ {
		idx, err := c.AcquireReader(context.Background(), 1)
		if err != nil {
			t.Fatalf("fill %d: %v", i, err)
		}
		slots = append(slots, idx)
	}
	released := make(chan struct{})
	go func() {
		defer close(released)
		time.Sleep(30 * time.Millisecond)
		c.ReleaseReader(slots[0])
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	idx, err := c.AcquireReader(ctx, 2)
	if err != nil {
		t.Fatalf("deadline acquire after free: %v", err)
	}
	// Join before the cleanup's File.Close: the acquire can win the
	// slot the instant the flock drops, while the releaser goroutine
	// is still returning through ReleaseReader.
	<-released
	c.ReleaseReader(idx)
}

func TestAcquireReaderConcurrentNoSlotAliasing(t *testing.T) {
	// N same-handle goroutines acquiring concurrently never alias a
	// slot: the Coord's acquisition mutex serializes them (two
	// try-locks through one hold description would not conflict).
	c, f := newTestCoord(t, 10*time.Millisecond)
	max := int(f.MaxReaders())
	var wg sync.WaitGroup
	got := make([]uint32, max)
	for i := 0; i < max; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			idx, err := c.AcquireReader(context.Background(), uint64(i+1))
			if err != nil {
				t.Errorf("acquire %d: %v", i, err)
				got[i] = NoSlot
				return
			}
			got[i] = idx
		}(i)
	}
	wg.Wait()
	seen := map[uint32]bool{}
	for i, idx := range got {
		if idx == NoSlot {
			continue
		}
		if seen[idx] {
			t.Fatalf("slot %d aliased (goroutine %d)", idx, i)
		}
		seen[idx] = true
	}
	for idx := range seen {
		c.ReleaseReader(idx)
	}
}

// The per-slot lock-FILE backend is darwin/freebsd production and
// the DST simulation tier, but newSlotLocks picks the range backend
// on linux — so these tests construct fileLocks directly to keep the
// portable backend covered on the real (linux) kernel.

func newTestFileLocks(t *testing.T) *fileLocks {
	t.Helper()
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	t.Cleanup(func() { _ = root.Close() })
	// Mirror the creator's eager population: the backend only opens.
	if err := populateReadersDir(OpenParams{Root: root, Base: "x.lock"}, 0xA5A5, 8); err != nil {
		t.Fatalf("populate: %v", err)
	}
	return &fileLocks{root: root, dir: readersDir("x.lock", 0xA5A5)}
}

func TestFileLocksHoldProbeConflict(t *testing.T) {
	// Each acquisition opens its own descriptor, so a probe conflicts
	// with a held lock even in-process — the property the range
	// backend needs a second description for.
	l := newTestFileLocks(t)
	release, err := l.tryHold(0)
	if err != nil {
		t.Fatalf("tryHold: %v", err)
	}
	if _, err := l.tryProbe(0); !errors.Is(err, errSlotBusy) {
		t.Fatalf("probe of held slot = %v, want errSlotBusy", err)
	}
	if _, err := l.tryHold(1); err != nil {
		t.Fatalf("tryHold of a different slot: %v", err)
	}
	release()
	rel2, err := l.tryProbe(0)
	if err != nil {
		t.Fatalf("probe after release = %v, want acquired", err)
	}
	rel2()
}

func TestPopulateSweepSkipsNonNonceEntries(t *testing.T) {
	// The creation sweep unlinks by pattern — uniquely in the
	// lifecycle — so the pattern is exact: directories with the
	// 8-hex-digit nonce suffix only. A sibling database whose NAME
	// happens to extend the prefix, or a stray file, is not ours.
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	defer root.Close()
	decoyFile := "x.lock.readers-cafecafe" // right shape, wrong kind
	if err := os.WriteFile(dir+"/"+decoyFile, []byte("peer"), 0o600); err != nil {
		t.Fatalf("decoy file: %v", err)
	}
	decoyDir := "x.lock.readers-zzzz.lock" // dir, non-nonce suffix
	if err := root.Mkdir(decoyDir, 0o755); err != nil {
		t.Fatalf("decoy dir: %v", err)
	}
	orphan := readersDir("x.lock", 0xDEAD)
	if err := root.Mkdir(orphan, 0o755); err != nil {
		t.Fatalf("orphan: %v", err)
	}
	if err := populateReadersDir(OpenParams{Root: root, Base: "x.lock"}, 0xBEEF, 2); err != nil {
		t.Fatalf("populate: %v", err)
	}
	if _, err := root.Stat(orphan); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("nonce-shaped orphan dir survived: %v", err)
	}
	if _, err := root.Stat(decoyFile); err != nil {
		t.Fatalf("decoy FILE was swept: %v", err)
	}
	if _, err := root.Stat(decoyDir); err != nil {
		t.Fatalf("non-nonce decoy dir was swept: %v", err)
	}
}

func TestPopulateReadersDirSweepsOrphans(t *testing.T) {
	// The creator's eager population first removes leftover readers
	// directories — orphans from crashed inits and superseded
	// incarnations a crashed removal missed. Anything present at
	// CREATE time is provably not the live incarnation.
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	defer root.Close()
	orphan := readersDir("x.lock", 0xDEAD)
	if err := root.Mkdir(orphan, 0o755); err != nil {
		t.Fatalf("mkdir orphan: %v", err)
	}
	if err := populateReadersDir(OpenParams{Root: root, Base: "x.lock"}, 0xBEEF, 4); err != nil {
		t.Fatalf("populate: %v", err)
	}
	if _, err := root.Stat(orphan); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("orphan dir survived creation sweep: %v", err)
	}
	for i := range uint32(4) {
		if _, err := root.Stat(fmt.Sprintf("%s/%d", readersDir("x.lock", 0xBEEF), i)); err != nil {
			t.Fatalf("slot file %d missing after populate: %v", i, err)
		}
	}
}

// failingSlotLocks errors every acquisition with a non-busy error.
type failingSlotLocks struct{ err error }

func (s *failingSlotLocks) tryHold(uint32) (func(), error)  { return nil, s.err }
func (s *failingSlotLocks) tryProbe(uint32) (func(), error) { return nil, s.err }
func (s *failingSlotLocks) close() error                    { return nil }

func TestReapReportsUndecidedProbes(t *testing.T) {
	// An occupied slot whose probe errors (not busy) can be neither
	// judged nor cleared: the residue keeps pinning the reclamation
	// bound, so the reap must SURFACE the count rather than swallow
	// it — a persistent undecided is a silently halted reclamation
	// (cross-process.md §Stale-slot reclamation).
	f := openTestFile(t, 2)
	Store64(&f.Slot(0).TxnID, 9)
	f.locks = &failingSlotLocks{err: errors.New("injected probe failure")}
	cleared, undecided := f.ReapStaleReaderSlots()
	if cleared != 0 {
		t.Fatalf("cleared %d under an undecided probe, want 0", cleared)
	}
	if undecided != 1 {
		t.Fatalf("undecided = %d, want 1", undecided)
	}
	if got := Load64(&f.Slot(0).TxnID); got != 9 {
		t.Fatalf("residue TxnID = %d, want 9 (never cleared on undecided)", got)
	}
}

func TestAcquireReaderSlotUndecidedIsNotFull(t *testing.T) {
	// A backend I/O failure is UNDECIDED — the scan must surface it,
	// never dress it as ErrReadersFull (which asserts every slot has
	// a live owner and steers callers into a hopeless retry loop).
	f := openTestFile(t, 2)
	ioErr := errors.New("injected backend failure")
	f.locks = &failingSlotLocks{err: ioErr}
	_, err := f.AcquireReaderSlot(0, 1, 42)
	if errors.Is(err, ErrReadersFull) {
		t.Fatalf("undecided scan reported ErrReadersFull")
	}
	if !errors.Is(err, ioErr) {
		t.Fatalf("err = %v, want the injected backend failure wrapped", err)
	}
}

// unlockOrderRecorder wraps a slotLocks backend and records the
// slot's TxnID at the moment each hold release fires.
type unlockOrderRecorder struct {
	inner slotLocks
	f     *File
	seen  []uint64
}

func (r *unlockOrderRecorder) tryHold(idx uint32) (func(), error) {
	rel, err := r.inner.tryHold(idx)
	if err != nil {
		return nil, err
	}
	return func() {
		r.seen = append(r.seen, Load64(&r.f.Slot(idx).TxnID))
		rel()
	}, nil
}
func (r *unlockOrderRecorder) tryProbe(idx uint32) (func(), error) { return r.inner.tryProbe(idx) }
func (r *unlockOrderRecorder) close() error                        { return r.inner.close() }

func TestReleaseZeroesBeforeUnlock(t *testing.T) {
	// cross-process.md §Slot release: zero-then-unlock. If the lock
	// dropped first, a peer could take the slot and publish its own
	// TxnID before this releaser's late zero landed — stomping the
	// new owner's published TxnID and unpinning its snapshot from
	// RPL reclamation (use-after-reclaim).
	f := openTestFile(t, 2)
	rec := &unlockOrderRecorder{inner: f.locks, f: f}
	f.locks = rec
	idx, err := f.AcquireReaderSlot(0, 7, 1)
	if err != nil {
		t.Fatalf("AcquireReaderSlot: %v", err)
	}
	f.ReleaseReaderSlot(idx)
	if len(rec.seen) != 1 {
		t.Fatalf("recorded %d hold releases, want 1", len(rec.seen))
	}
	if rec.seen[0] != 0 {
		t.Fatalf("TxnID at unlock = %d, want 0 (zero-before-unlock)", rec.seen[0])
	}
}

func TestRangeLocksHoldProbeConflict(t *testing.T) {
	// The range backend's probe description must be DISTINCT from its
	// hold description: OFD try-locks through one description never
	// conflict with themselves, so a hold-description probe would
	// judge this File's own live reader dead. The reap's own-held
	// guard usually masks that — but not in the acquire window
	// between a slot's tryHold and its holds registration, where a
	// concurrent same-File reap probes a just-taken live slot.
	f := openTestFile(t, 2)
	if _, ok := f.locks.(*rangeLocks); !ok {
		t.Skip("range backend not in use on this platform/build")
	}
	idx, err := f.AcquireReaderSlot(0, 7, 1)
	if err != nil {
		t.Fatalf("AcquireReaderSlot: %v", err)
	}
	if _, err := f.locks.tryProbe(idx); !errors.Is(err, errSlotBusy) {
		t.Fatalf("same-File probe of own held slot = %v, want errSlotBusy (distinct descriptions)", err)
	}
}

func TestReapSerializedPerFile(t *testing.T) {
	// Same-File clearers share ONE probe description, which cannot
	// conflict with itself — the kernel cannot serialize them, so
	// ReapStaleReaderSlots must (cross-process.md §Stale-slot
	// reclamation). Without that, reaper A's probe release strips
	// reaper B's "held" probe mid-clear, and B's late zero-store can
	// erase a slot a new reader has since taken. Pinned directly:
	// while one reap is parked inside a clear, a second same-File
	// reap must not complete.
	f := openTestFile(t, 2)
	Store64(&f.Slot(0).TxnID, 9) // dead residue

	parked := make(chan struct{})
	unpark := make(chan struct{})
	var once sync.Once
	hook := func(uint32) {
		once.Do(func() {
			close(parked)
			<-unpark
		})
	}
	staleClearHookForTest.Store(&hook)
	t.Cleanup(func() { staleClearHookForTest.Store(nil) })

	aDone := make(chan int, 1)
	go func() { c, _ := f.ReapStaleReaderSlots(); aDone <- c }()
	<-parked
	bDone := make(chan int, 1)
	go func() { c, _ := f.ReapStaleReaderSlots(); bDone <- c }()
	select {
	case <-bDone:
		t.Fatal("second same-File reap completed while the first was mid-clear (clearers not serialized)")
	case <-time.After(50 * time.Millisecond):
	}
	close(unpark)
	if cleared := <-aDone; cleared != 1 {
		t.Errorf("first reap cleared %d, want 1", cleared)
	}
	<-bDone
}

// passOneBusyOnce simulates a peer that holds slot 0 during the
// acquire scan's first pass and releases before the second.
type passOneBusyOnce struct {
	inner slotLocks
	fired bool
}

func (s *passOneBusyOnce) tryHold(idx uint32) (func(), error) {
	if !s.fired && idx == 0 {
		s.fired = true
		return nil, errSlotBusy
	}
	return s.inner.tryHold(idx)
}
func (s *passOneBusyOnce) tryProbe(idx uint32) (func(), error) { return s.inner.tryProbe(idx) }
func (s *passOneBusyOnce) close() error                        { return s.inner.close() }

func TestAcquireRetriesFreedSlotInPassTwo(t *testing.T) {
	// A peer holding the only slot during pass 1 and releasing before
	// pass 2 must not surface ErrReadersFull: full means every slot's
	// lock is HELD (cross-process.md §Slot acquire), never "the table
	// churned mid-scan". Pass 2 therefore try-locks every slot, not
	// only the nonzero ones — a zero slot pass 1 lost to a live
	// holder is retried, not skipped.
	f := openTestFile(t, 1)
	f.locks = &passOneBusyOnce{inner: f.locks}
	idx, err := f.AcquireReaderSlot(0, 7, 1)
	if err != nil {
		t.Fatalf("acquire across the pass boundary: %v (want slot 0)", err)
	}
	if idx != 0 {
		t.Fatalf("landed on %d, want 0", idx)
	}
}

func TestRaiseReaderSlotTxnIDUnownedPanics(t *testing.T) {
	// Owner-only precondition, enforced: raising a slot this File
	// does not hold would stomp another owner's pin.
	f := openTestFile(t, 2)
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic raising an unowned slot")
		}
	}()
	f.RaiseReaderSlotTxnID(0, 9)
}

func TestReadersDirScopedToIncarnation(t *testing.T) {
	// The per-slot lock-file directory is named by the header's
	// incarnation nonce, so a recreated lock file (UUID mismatch,
	// stale format) derives a DIFFERENT directory — however the
	// filesystem reuses inodes — and a prior incarnation's surviving
	// holders cannot wedge the fresh table by holding locks on
	// same-named slot files (cross-process.md §Reader Table). Two
	// handles of ONE incarnation must agree on the name; two
	// incarnations must not (nonce collision odds 2^-32 — a failure
	// here is a real regression, not flake).
	root, base, fullPath := tmpLock(t)
	params := OpenParams{Root: root, Base: base, DataUUID: [16]byte{0xD1}, MaxReaders: 2}
	f1, err := Open(params)
	if err != nil {
		t.Fatalf("Open 1: %v", err)
	}
	nonce1 := f1.header.ReadersDirNonce
	f2, err := Open(params)
	if err != nil {
		t.Fatalf("Open 2 (same incarnation): %v", err)
	}
	if got := f2.header.ReadersDirNonce; got != nonce1 {
		t.Fatalf("same-incarnation adopter read nonce %08x, creator stamped %08x", got, nonce1)
	}
	f2.Close()
	f1.Close()
	if err := os.Remove(fullPath); err != nil {
		t.Fatalf("remove: %v", err)
	}
	f3, err := Open(params)
	if err != nil {
		t.Fatalf("Open 3 (new incarnation): %v", err)
	}
	defer f3.Close()
	nonce3 := f3.header.ReadersDirNonce
	if nonce3 == nonce1 {
		t.Fatalf("recreated lock file stamped the same nonce %08x", nonce1)
	}
	if readersDir(base, nonce1) == readersDir(base, nonce3) {
		t.Fatalf("distinct nonces derived the same readers dir")
	}
}

func TestFileLocksDirRemovedFailsClosed(t *testing.T) {
	// Externally removing a LIVE incarnation's readers dir is outside
	// the protection boundary: recreating it would mint fresh
	// slot-file inodes while a surviving holder's lock rides the
	// unlinked one — a silent double-claim. The open path must fail
	// CLOSED (undecided error), never silently recreate
	// (cross-process.md §Reader Table, slot locks).
	l := newTestFileLocks(t)
	rel, err := l.tryHold(0)
	if err != nil {
		t.Fatalf("tryHold: %v", err)
	}
	defer rel() // held across the sweep — the dangerous interleaving
	if err := os.RemoveAll(l.root.Name() + "/" + l.dir); err != nil {
		t.Fatalf("remove dir: %v", err)
	}
	if _, err := l.tryHold(1); err == nil || errors.Is(err, errSlotBusy) {
		t.Fatalf("tryHold after live-dir sweep = %v, want a fail-closed undecided error", err)
	}
	// And no silent double-claim of the HELD slot through a peer
	// handle either: its acquisition must also fail closed.
	peer := &fileLocks{root: l.root, dir: l.dir}
	if _, err := peer.tryHold(0); err == nil || errors.Is(err, errSlotBusy) {
		t.Fatalf("peer tryHold after live-dir sweep = %v, want a fail-closed undecided error", err)
	}
}

func TestFileLocksSlotFileRemovedFailsClosed(t *testing.T) {
	// The single-slot-file variant of the sweep hazard: with the
	// directory intact, an O_CREATE in the open path would silently
	// mint a fresh inode for a swept slot file while the surviving
	// holder's lock rides the unlinked one — the peer would
	// flock-acquire a slot that is still HELD. The open path must
	// fail closed on the missing file too.
	l := newTestFileLocks(t)
	rel, err := l.tryHold(0)
	if err != nil {
		t.Fatalf("tryHold: %v", err)
	}
	defer rel()
	if err := os.Remove(l.root.Name() + "/" + l.dir + "/0"); err != nil {
		t.Fatalf("remove slot file: %v", err)
	}
	peer := &fileLocks{root: l.root, dir: l.dir}
	if _, err := peer.tryHold(0); err == nil || errors.Is(err, errSlotBusy) {
		t.Fatalf("peer tryHold of a swept-but-held slot file = %v, want a fail-closed undecided error", err)
	}
}

func TestStaleRemovalSweepsReadersDir(t *testing.T) {
	// The guarded stale removal deletes the outgoing incarnation's
	// readers directory together with its lock file — the only
	// sanctioned removal, run exactly when the incarnation is
	// provably superseded, so litter never invites external sweeps
	// (cross-process.md §Reader Table, slot locks).
	root, base, fullPath := tmpLock(t)
	f, err := Open(OpenParams{Root: root, Base: base, DataUUID: [16]byte{0xA1}, MaxReaders: 2})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	dir := readersDir(base, f.header.ReadersDirNonce)
	// On the file-backend tier the creator populated the dir eagerly;
	// on the range tier it does not exist yet. Either is fine — the
	// stale removal must take it out both ways.
	if err := root.Mkdir(dir, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(fullPath+"-litter-probe", nil, 0o600); err != nil {
		t.Fatalf("marker: %v", err)
	}
	if err := os.WriteFile(root.Name()+"/"+dir+"/0", nil, 0o600); err != nil {
		t.Fatalf("slot file: %v", err)
	}
	f.Close()

	// A different DataUUID classifies the file stale: the removal
	// must take the directory with it.
	f2, err := Open(OpenParams{Root: root, Base: base, DataUUID: [16]byte{0xB2}, MaxReaders: 2})
	if err != nil {
		t.Fatalf("Open (new uuid): %v", err)
	}
	defer f2.Close()
	if _, err := root.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("outgoing readers dir still present after stale removal: %v", err)
	}
}

func TestSlotLocksRejectReboundName(t *testing.T) {
	// The slot-lock descriptions are opened BY NAME after the
	// lifecycle's identity verify; a name re-bound in that window
	// (an unguarded remover racing the open) must be caught — a
	// description on a different inode than the mmap would lock a
	// file whose table this handle does not read (split brain).
	// newSlotLocks verifies each description against the validated
	// fd and reports errPathChanged so the lifecycle retries.
	root, base, fullPath := tmpLock(t)
	if err := os.WriteFile(fullPath, []byte("old"), 0o600); err != nil {
		t.Fatalf("create: %v", err)
	}
	f, err := root.OpenFile(base, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open validated fd: %v", err)
	}
	defer f.Close()
	// Re-bind the name to a different inode behind the fd's back.
	if err := os.Remove(fullPath); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := os.WriteFile(fullPath, []byte("new"), 0o600); err != nil {
		t.Fatalf("recreate: %v", err)
	}
	locks, err := newSlotLocks(OpenParams{Root: root, Base: base}, f, 1)
	if flock.RangeSupported {
		if !errors.Is(err, errPathChanged) {
			if locks != nil {
				_ = locks.close()
			}
			t.Fatalf("newSlotLocks on a re-bound name = %v, want errPathChanged", err)
		}
		return
	}
	// File backend: identity is consumed as the dir name instead —
	// the incarnation-scoping test covers it.
	if err != nil {
		t.Fatalf("newSlotLocks (file backend): %v", err)
	}
	_ = locks.close()
}

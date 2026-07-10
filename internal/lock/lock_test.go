package lock

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
)

// tmpLock returns an *os.Root over a fresh per-test temp directory
// plus the conventional base name for the test's lock file. The
// Root is registered for cleanup at test end. Tests that also need
// filesystem-level access (raw byte tamper, truncate-from-outside)
// can derive the full path via filepath.Join(root_dir, base) using
// the dir captured separately.
func tmpLock(t *testing.T) (*os.Root, string, string) {
	t.Helper()
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	t.Cleanup(func() { root.Close() })
	return root, "db.gmdb.lock", filepath.Join(dir, "db.gmdb.lock")
}

func TestFileSize(t *testing.T) {
	cases := []struct {
		maxReaders uint32
		want       int64
	}{
		{1, int64(HeaderSize) + int64(SlotSize)},
		{2, int64(HeaderSize) + 2*int64(SlotSize)},
		{DefaultMaxReaders, int64(HeaderSize) + int64(SlotSize)*int64(DefaultMaxReaders)},
		{MaxMaxReaders, int64(HeaderSize) + int64(SlotSize)*int64(MaxMaxReaders)},
	}
	for _, c := range cases {
		if got := FileSize(c.maxReaders); got != c.want {
			t.Errorf("FileSize(%d) = %d, want %d", c.maxReaders, got, c.want)
		}
	}
}

func TestStructSizes(t *testing.T) {
	if HeaderSize != 112 {
		t.Errorf("HeaderSize = %d, want 112", HeaderSize)
	}
	if SlotSize != 48 {
		t.Errorf("SlotSize = %d, want 48", SlotSize)
	}
}

func TestOpenCreates(t *testing.T) {
	root, base, fullPath := tmpLock(t)
	uuid := [16]byte{0xAA, 0xBB, 0xCC, 0xDD}

	f, err := Open(OpenParams{Root: root, Base: base, DataUUID: uuid, MaxReaders: 8})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer f.Close()

	if got := f.MaxReaders(); got != 8 {
		t.Errorf("MaxReaders = %d, want 8", got)
	}
	if got := f.UUID(); got != uuid {
		t.Errorf("UUID = %x, want %x", got, uuid)
	}
	if got := f.WriterPID(); got != 0 {
		t.Errorf("fresh WriterPID = %d, want 0", got)
	}
	if got := f.WriterHeartbeat(); got != 0 {
		t.Errorf("fresh WriterHeartbeat = %d, want 0", got)
	}
	for i := range uint32(8) {
		slot := f.Slot(i)
		if Load64(&slot.TxnID) != 0 {
			t.Errorf("fresh slot %d TxnID != 0", i)
		}
		if Load64(&slot.PID) != 0 {
			t.Errorf("fresh slot %d PID != 0", i)
		}
	}
	st, err := os.Stat(fullPath)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if st.Size() != FileSize(8) {
		t.Errorf("on-disk size = %d, want %d", st.Size(), FileSize(8))
	}
}

func TestOpenReopens(t *testing.T) {
	root, base, _ := tmpLock(t)
	uuid := [16]byte{1, 2, 3, 4}

	f1, err := Open(OpenParams{Root: root, Base: base, DataUUID: uuid, MaxReaders: 16})
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	f1.SetWriterPID(12345)
	if err := f1.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}

	// Reopen with a different caller MaxReaders — header value is
	// authoritative.
	f2, err := Open(OpenParams{Root: root, Base: base, DataUUID: uuid, MaxReaders: 32})
	if err != nil {
		t.Fatalf("re-Open: %v", err)
	}
	defer f2.Close()
	if got := f2.MaxReaders(); got != 16 {
		t.Errorf("re-opened MaxReaders = %d, want 16 (header authoritative)", got)
	}
	if got := f2.WriterPID(); got != 12345 {
		t.Errorf("re-opened WriterPID = %d, want 12345 (state preserved)", got)
	}
}

func TestOpenUUIDMismatchRecreates(t *testing.T) {
	root, base, _ := tmpLock(t)
	uuidA := [16]byte{0xAA}
	uuidB := [16]byte{0xBB}

	fA, err := Open(OpenParams{Root: root, Base: base, DataUUID: uuidA, MaxReaders: 8})
	if err != nil {
		t.Fatalf("Open uuidA: %v", err)
	}
	fA.SetWriterPID(99)
	fA.Close()

	fB, err := Open(OpenParams{Root: root, Base: base, DataUUID: uuidB, MaxReaders: 8})
	if err != nil {
		t.Fatalf("Open uuidB: %v", err)
	}
	defer fB.Close()

	if got := fB.UUID(); got != uuidB {
		t.Errorf("recreated UUID = %x, want %x", got, uuidB)
	}
	if got := fB.WriterPID(); got != 0 {
		t.Errorf("recreated WriterPID = %d, want 0 (stale state must not persist)", got)
	}
}

func TestOpenRejectsInvalidMaxReaders(t *testing.T) {
	root, base, _ := tmpLock(t)
	uuid := [16]byte{0xAA}
	cases := []uint32{0, MaxMaxReaders + 1}
	for _, mr := range cases {
		_, err := Open(OpenParams{Root: root, Base: base, DataUUID: uuid, MaxReaders: mr})
		if !errors.Is(err, ErrInvalidMaxReaders) {
			t.Errorf("MaxReaders=%d: got %v, want ErrInvalidMaxReaders", mr, err)
		}
	}
}

func TestOpenRejectsInvalidBase(t *testing.T) {
	root, _, _ := tmpLock(t)
	uuid := [16]byte{0xAA}
	cases := []string{"foo/bar.lock", "../escape.lock", "with\x00null"}
	for _, base := range cases {
		_, err := Open(OpenParams{Root: root, Base: base, DataUUID: uuid, MaxReaders: 8})
		if !errors.Is(err, ErrInvalidBase) {
			t.Errorf("Base=%q: got %v, want ErrInvalidBase", base, err)
		}
	}
}

func TestOpenRejectsNilRoot(t *testing.T) {
	_, err := Open(OpenParams{Root: nil, Base: "x.lock", DataUUID: [16]byte{1}, MaxReaders: 8})
	if err == nil {
		t.Errorf("Root=nil: got nil error")
	}
}

func TestOpenRejectsCorruptMagic(t *testing.T) {
	root, base, fullPath := tmpLock(t)
	uuid := [16]byte{0xAA}

	if err := os.WriteFile(fullPath, make([]byte, FileSize(8)), 0o600); err != nil {
		t.Fatalf("write bogus file: %v", err)
	}

	_, err := Open(OpenParams{Root: root, Base: base, DataUUID: uuid, MaxReaders: 8})
	if !errors.Is(err, ErrCorrupted) {
		t.Errorf("got %v, want ErrCorrupted", err)
	}
}

func TestOpenRejectsCorruptMaxReaders(t *testing.T) {
	root, base, fullPath := tmpLock(t)
	uuid := [16]byte{0xAA}

	f, err := Open(OpenParams{Root: root, Base: base, DataUUID: uuid, MaxReaders: 8})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	f.Close()

	fd, err := os.OpenFile(fullPath, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open for tamper: %v", err)
	}
	bigEnough := MaxMaxReaders + 100
	buf := []byte{
		byte(bigEnough), byte(bigEnough >> 8),
		byte(bigEnough >> 16), byte(bigEnough >> 24),
	}
	if _, err := fd.WriteAt(buf, 8); err != nil {
		t.Fatalf("tamper write: %v", err)
	}
	fd.Close()

	_, err = Open(OpenParams{Root: root, Base: base, DataUUID: uuid, MaxReaders: 8})
	if !errors.Is(err, ErrCorrupted) {
		t.Errorf("got %v, want ErrCorrupted", err)
	}
}

func TestOpenRecreatesOnSizeMismatch(t *testing.T) {
	// A plausible header (magic + MaxReaders in range) whose file size
	// disagrees with the current layout is a lock file written by a
	// binary with a different header layout (pre-v1 layout changes grow
	// HeaderSize in place). The lock file is transient coordination
	// state: Open treats it as stale — unlink + recreate — exactly like
	// a UUID mismatch, rather than refusing to open an intact database.
	root, base, fullPath := tmpLock(t)
	uuid := [16]byte{0xAA}

	f, err := Open(OpenParams{Root: root, Base: base, DataUUID: uuid, MaxReaders: 8})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	f.Close()

	if err := os.Truncate(fullPath, FileSize(8)+SlotSize); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	f2, err := Open(OpenParams{Root: root, Base: base, DataUUID: uuid, MaxReaders: 8})
	if err != nil {
		t.Fatalf("reopen after size mismatch: %v (want stale-recreate)", err)
	}
	defer f2.Close()
	st, err := os.Stat(fullPath)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if st.Size() != FileSize(8) {
		t.Errorf("recreated size = %d, want %d", st.Size(), FileSize(8))
	}
}

func TestOpenRejectsUndersizedFile(t *testing.T) {
	// With the flock-during-init lifecycle, a partially-
	// initialised file can only come from external tampering or a
	// crashed creator — never from a live concurrent creator. The
	// lock surface treats both as ErrCorrupted (no auto-recover)
	// rather than silently unlinking, since auto-unlink could
	// destroy a legitimate user's mid-init progress in a future
	// design where init takes longer.
	root, base, fullPath := tmpLock(t)
	uuid := [16]byte{0xAA}

	if err := os.WriteFile(fullPath, []byte{0xFF}, 0o600); err != nil {
		t.Fatalf("write tiny file: %v", err)
	}

	_, err := Open(OpenParams{Root: root, Base: base, DataUUID: uuid, MaxReaders: 8})
	if !errors.Is(err, ErrCorrupted) {
		t.Errorf("got %v, want ErrCorrupted", err)
	}
}

func TestAtomicHelpers(t *testing.T) {
	root, base, _ := tmpLock(t)
	f, err := Open(OpenParams{Root: root, Base: base, DataUUID: [16]byte{1}, MaxReaders: 4})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer f.Close()

	slot := f.Slot(2)

	Store64(&slot.TxnID, 0xDEADBEEF)
	if got := Load64(&slot.TxnID); got != 0xDEADBEEF {
		t.Errorf("Store64/Load64 round-trip: got %x, want 0xDEADBEEF", got)
	}

	if !CAS64(&slot.TxnID, 0xDEADBEEF, 0x12345) {
		t.Error("CAS64 should have succeeded")
	}
	if got := Load64(&slot.TxnID); got != 0x12345 {
		t.Errorf("post-CAS Load = %x, want 0x12345", got)
	}

	if CAS64(&slot.TxnID, 0xDEADBEEF, 0x67890) {
		t.Error("CAS64 should have failed")
	}
	if got := Load64(&slot.TxnID); got != 0x12345 {
		t.Errorf("failed-CAS modified value: got %x, want 0x12345", got)
	}
}

func TestHeaderAccessors(t *testing.T) {
	root, base, _ := tmpLock(t)
	f, err := Open(OpenParams{Root: root, Base: base, DataUUID: [16]byte{1}, MaxReaders: 4})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer f.Close()

	f.SetWriterPID(7777)
	f.SetWriterStartTime(0x1111)
	f.SetWriterPIDNamespace(0x2222)
	f.SetWriterHeartbeat(0x3333)
	f.SetLastMaintenanceTime(0x4444)

	if got := f.WriterPID(); got != 7777 {
		t.Errorf("WriterPID = %d, want 7777", got)
	}
	if got := f.WriterStartTime(); got != 0x1111 {
		t.Errorf("WriterStartTime = %x", got)
	}
	if got := f.WriterPIDNamespace(); got != 0x2222 {
		t.Errorf("WriterPIDNamespace = %x", got)
	}
	if got := f.WriterHeartbeat(); got != 0x3333 {
		t.Errorf("WriterHeartbeat = %x", got)
	}
	if got := f.LastMaintenanceTime(); got != 0x4444 {
		t.Errorf("LastMaintenanceTime = %x", got)
	}
}

func TestHeaderPersistsAcrossReopen(t *testing.T) {
	root, base, _ := tmpLock(t)
	uuid := [16]byte{0xAA}

	f1, err := Open(OpenParams{Root: root, Base: base, DataUUID: uuid, MaxReaders: 4})
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	f1.SetWriterPID(42)
	f1.SetWriterStartTime(7)
	Store64(&f1.Slot(1).TxnID, 0xABCD)
	if err := f1.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}

	f2, err := Open(OpenParams{Root: root, Base: base, DataUUID: uuid, MaxReaders: 4})
	if err != nil {
		t.Fatalf("re-Open: %v", err)
	}
	defer f2.Close()

	if got := f2.WriterPID(); got != 42 {
		t.Errorf("re-opened WriterPID = %d, want 42", got)
	}
	if got := f2.WriterStartTime(); got != 7 {
		t.Errorf("re-opened WriterStartTime = %d, want 7", got)
	}
	if got := Load64(&f2.Slot(1).TxnID); got != 0xABCD {
		t.Errorf("re-opened slot 1 TxnID = %x, want 0xABCD", got)
	}
}

func TestSlotIndexOutOfRangePanics(t *testing.T) {
	root, base, _ := tmpLock(t)
	f, err := Open(OpenParams{Root: root, Base: base, DataUUID: [16]byte{1}, MaxReaders: 4})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer f.Close()

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("Slot(MaxReaders) should panic")
		}
	}()
	_ = f.Slot(4)
}

func TestCloseIdempotent(t *testing.T) {
	root, base, _ := tmpLock(t)
	f, err := Open(OpenParams{Root: root, Base: base, DataUUID: [16]byte{1}, MaxReaders: 4})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Errorf("first Close: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

func TestPostCloseAccessorsPanic(t *testing.T) {
	// The lifetime contract on *File: after Close, accessors panic
	// rather than nil-deref'ing into unmapped memory or returning a
	// silently-wrong zero value. This pins the contract.
	root, base, _ := tmpLock(t)
	f, err := Open(OpenParams{Root: root, Base: base, DataUUID: [16]byte{1}, MaxReaders: 4})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	checkPanic := func(t *testing.T, fn func()) {
		t.Helper()
		defer func() {
			if r := recover(); r == nil {
				t.Errorf("expected panic; got none")
			}
		}()
		fn()
	}
	t.Run("Fd", func(t *testing.T) { checkPanic(t, func() { _ = f.Fd() }) })
	t.Run("MaxReaders", func(t *testing.T) { checkPanic(t, func() { _ = f.MaxReaders() }) })
	t.Run("UUID", func(t *testing.T) { checkPanic(t, func() { _ = f.UUID() }) })
	t.Run("Slot", func(t *testing.T) { checkPanic(t, func() { _ = f.Slot(0) }) })
	t.Run("WriterPID", func(t *testing.T) { checkPanic(t, func() { _ = f.WriterPID() }) })
	t.Run("SetWriterPID", func(t *testing.T) { checkPanic(t, func() { f.SetWriterPID(1) }) })
	t.Run("WriterHeartbeat", func(t *testing.T) { checkPanic(t, func() { _ = f.WriterHeartbeat() }) })
}

func TestConcurrentOpenRaceWithCrossMmapVisibility(t *testing.T) {
	// Regression for the flock-during-init lifecycle fix: N goroutines
	// race the same lock-file path. Exactly one wins the
	// O_CREATE|O_EXCL race and inits under flock(LOCK_EX); the others
	// take flock(LOCK_SH) and block on the initialiser's lock until
	// init completes — so every adopter sees a fully-published
	// header, never a partially-init'd one.
	//
	// Cross-mmap-visibility post-condition: writes via one *File are
	// visible via every other *File (MAP_SHARED page-cache coherent).
	// If any opener got a different inode (the pre-fix split-brain
	// failure mode), the SetWriterPID via files[0] would not be
	// observable from files[1..N-1].
	root, base, _ := tmpLock(t)
	uuid := [16]byte{0xAA}

	const N = 10
	files := make([]*File, N)
	errs := make([]error, N)
	var start sync.WaitGroup
	var done sync.WaitGroup
	start.Add(1)
	done.Add(N)
	for i := range N {
		go func() {
			defer done.Done()
			start.Wait()
			files[i], errs[i] = Open(OpenParams{
				Root: root, Base: base, DataUUID: uuid, MaxReaders: 8,
			})
		}()
	}
	start.Done()
	done.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: %v", i, err)
		}
		if files[i] == nil {
			t.Fatalf("goroutine %d: nil file with nil error", i)
		}
	}

	// Visibility check — the H1 failure mode produces split-brain
	// where some openers hold a *File backed by a different inode
	// than the others. If split-brain occurred, the SetWriterPID
	// via files[0] would NOT be observable from the rest.
	files[0].SetWriterPID(0xCAFE)
	for i := 1; i < N; i++ {
		if got := files[i].WriterPID(); got != 0xCAFE {
			t.Errorf("file %d WriterPID = %x, want 0xCAFE (split-brain: distinct mmap)", i, got)
		}
	}

	for _, f := range files {
		_ = f.Close()
	}
}

func TestBaseFor(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"/tmp/foo.gmdb", "foo.gmdb.lock"},
		{"bar", "bar.lock"},
		{"./relative/path.gmdb", "path.gmdb.lock"},
	}
	for _, c := range cases {
		if got := BaseFor(c.in); got != c.want {
			t.Errorf("BaseFor(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// A stale-file removal must never unlink a lock file it did not
// validate: two concurrent openers hitting a stale file otherwise end
// up on two different lock files — two simultaneous writers on one
// data file (meta overwrite, page aliasing). The hook models the
// losing opener's window: it validated the stale inode, and before
// its removal runs, the WINNING opener has already removed the stale
// file and created the fresh one. The loser's guarded removal must
// detect the re-bound name, skip, and adopt the winner's file.
func TestOpenStaleRemovalSkipsRecreatedLockFile(t *testing.T) {
	root, base, _ := tmpLock(t)
	uuidStale := [16]byte{0xAA}
	uuidLive := [16]byte{0xBB}

	// The stale leftover (previous database at this path).
	fS, err := Open(OpenParams{Root: root, Base: base, DataUUID: uuidStale, MaxReaders: 8})
	if err != nil {
		t.Fatalf("Open stale: %v", err)
	}
	fS.Close()

	// The hook fires inside the guarded removal, after the flock
	// upgrade, before the identity re-check — simulating the winner
	// completing its remove+recreate in that window.
	var winner *File
	var winnerInfo os.FileInfo
	fired := false
	hook := func() {
		if fired {
			return
		}
		fired = true
		if err := root.Remove(base); err != nil {
			t.Errorf("winner remove: %v", err)
			return
		}
		w, err := Open(OpenParams{Root: root, Base: base, DataUUID: uuidLive, MaxReaders: 8})
		if err != nil {
			t.Errorf("winner create: %v", err)
			return
		}
		winner = w
		st, err := root.Stat(base)
		if err != nil {
			t.Errorf("winner stat: %v", err)
			return
		}
		winnerInfo = st
	}
	restore := SetStaleRemoveHookForTest(hook)
	defer restore()

	// The losing opener: validates the stale inode, must NOT unlink
	// the winner's file, and must converge onto it.
	fB, err := Open(OpenParams{Root: root, Base: base, DataUUID: uuidLive, MaxReaders: 8})
	if err != nil {
		t.Fatalf("Open loser: %v", err)
	}
	defer fB.Close()
	if winner != nil {
		defer winner.Close()
	}
	if winnerInfo == nil {
		t.Fatal("fixture: winner never created its lock file")
	}
	// The winner's inode must still be what the path names — the
	// pre-guard code unlinked it here, splitting the fleet across two
	// lock files.
	pInfo, err := root.Stat(base)
	if err != nil {
		t.Fatalf("stat after loser Open: %v", err)
	}
	if !os.SameFile(winnerInfo, pInfo) {
		t.Fatalf("winner's lock file was unlinked by the losing opener's stale removal — split brain")
	}
	// And the loser coordinates on that same inode.
	if got := fB.UUID(); got != uuidLive {
		t.Errorf("loser adopted UUID %x, want %x", got, uuidLive)
	}
}

// The guard's name-already-gone arm: the winner removed the stale
// file but has not (yet) recreated it — the loser must skip the
// removal (nothing to remove) and converge by creating fresh.
func TestOpenStaleRemovalSkipsWhenNameAlreadyGone(t *testing.T) {
	root, base, _ := tmpLock(t)
	fS, err := Open(OpenParams{Root: root, Base: base, DataUUID: [16]byte{0xAA}, MaxReaders: 8})
	if err != nil {
		t.Fatalf("Open stale: %v", err)
	}
	fS.Close()
	fired := false
	restore := SetStaleRemoveHookForTest(func() {
		if fired {
			return
		}
		fired = true
		if err := root.Remove(base); err != nil {
			t.Errorf("winner remove: %v", err)
		}
	})
	defer restore()
	fB, err := Open(OpenParams{Root: root, Base: base, DataUUID: [16]byte{0xBB}, MaxReaders: 8})
	if err != nil {
		t.Fatalf("Open after name-gone skip: %v", err)
	}
	defer fB.Close()
	if got := fB.UUID(); got != ([16]byte{0xBB}) {
		t.Errorf("UUID = %x, want BB...", got)
	}
}

// The guard's contention arm: a live holder of the stale inode's
// flock (a legacy coordinator of the abandoned database) makes the
// LOCK_EX|LOCK_NB conversion fail — the opener must SKIP the removal
// (never unlink under a live user) and, with the holder never
// releasing, exhaust its budget without removing the file.
func TestOpenStaleRemovalSkipsUnderLiveFlockHolder(t *testing.T) {
	root, base, path := tmpLock(t)
	fS, err := Open(OpenParams{Root: root, Base: base, DataUUID: [16]byte{0xAA}, MaxReaders: 8})
	if err != nil {
		t.Fatalf("Open stale: %v", err)
	}
	fS.Close()
	// The legacy holder: an independent fd with LOCK_SH held for the
	// duration (blocks every EX conversion).
	holder, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("holder open: %v", err)
	}
	defer holder.Close()
	if err := syscall.Flock(int(holder.Fd()), syscall.LOCK_SH); err != nil {
		t.Fatalf("holder flock: %v", err)
	}

	_, err = Open(OpenParams{Root: root, Base: base, DataUUID: [16]byte{0xBB}, MaxReaders: 8})
	if err == nil {
		t.Fatal("Open succeeded despite a live flock holder on the stale file")
	}
	if !errors.Is(err, errStaleContended) {
		t.Fatalf("budget exhausted by %v, want errStaleContended", err)
	}
	// The stale file must have survived every attempt.
	if _, statErr := root.Stat(base); statErr != nil {
		t.Fatalf("stale file removed under a live flock holder: %v", statErr)
	}
	fCheck, err := os.OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		t.Fatalf("reopen stale: %v", err)
	}
	defer fCheck.Close()
}

// The size-mismatch stale arm routes through the same identity guard
// as the UUID arm.
func TestOpenSizeMismatchRoutesThroughGuardedRemoval(t *testing.T) {
	root, base, path := tmpLock(t)
	fS, err := Open(OpenParams{Root: root, Base: base, DataUUID: [16]byte{0xAA}, MaxReaders: 8})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	fS.Close()
	// Grow the file past its layout's size: plausible header, wrong
	// size — the old-binary-layout classification.
	if err := os.Truncate(path, FileSize(8)+4096); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	hookFired := false
	restore := SetStaleRemoveHookForTest(func() { hookFired = true })
	defer restore()
	fB, err := Open(OpenParams{Root: root, Base: base, DataUUID: [16]byte{0xAA}, MaxReaders: 8})
	if err != nil {
		t.Fatalf("Open after size mismatch: %v", err)
	}
	defer fB.Close()
	if !hookFired {
		t.Fatal("size-mismatch removal did not route through the identity guard")
	}
}

// verifyPathIdentity unit pins: same inode passes; re-bound and
// removed names report errPathChanged.
func TestVerifyPathIdentity(t *testing.T) {
	root, base, _ := tmpLock(t)
	f, err := Open(OpenParams{Root: root, Base: base, DataUUID: [16]byte{0xAA}, MaxReaders: 8})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer f.Close()
	p := OpenParams{Root: root, Base: base}
	if err := verifyPathIdentity(p, f.f); err != nil {
		t.Fatalf("same inode: %v", err)
	}
	if err := root.Remove(base); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := verifyPathIdentity(p, f.f); !errors.Is(err, errPathChanged) {
		t.Fatalf("name gone: err = %v, want errPathChanged", err)
	}
	f2, err := Open(OpenParams{Root: root, Base: base, DataUUID: [16]byte{0xBB}, MaxReaders: 8})
	if err != nil {
		t.Fatalf("recreate: %v", err)
	}
	defer f2.Close()
	if err := verifyPathIdentity(p, f.f); !errors.Is(err, errPathChanged) {
		t.Fatalf("name re-bound: err = %v, want errPathChanged", err)
	}
}

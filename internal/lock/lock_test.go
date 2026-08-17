package lock

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
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
		{1, int64(HeaderSize) + int64(SlotSize) + NotifyRegionSize},
		{2, int64(HeaderSize) + 2*int64(SlotSize) + NotifyRegionSize},
		{DefaultMaxReaders, int64(HeaderSize) + int64(SlotSize)*int64(DefaultMaxReaders) + NotifyRegionSize},
		{MaxMaxReaders, int64(HeaderSize) + int64(SlotSize)*int64(MaxMaxReaders) + NotifyRegionSize},
	}
	for _, c := range cases {
		if got := FileSize(c.maxReaders); got != c.want {
			t.Errorf("FileSize(%d) = %d, want %d", c.maxReaders, got, c.want)
		}
	}
}

func TestStructSizes(t *testing.T) {
	if HeaderSize != 144 {
		t.Errorf("HeaderSize = %d, want 144", HeaderSize)
	}
	if SlotSize != 56 {
		t.Errorf("SlotSize = %d, want 56", SlotSize)
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

	// A NON-ZERO wrong magic is a finalised-but-invalid header —
	// terminal corruption, no recovery, no retry budget burned. (An
	// all-zero file is the crashed-creator staleness class instead —
	// TestOpenRecoversTornInit.)
	bogus := make([]byte, FileSize(8))
	copy(bogus, []byte{0xDE, 0xAD, 0xBE, 0xEF, 0xDE, 0xAD, 0xBE, 0xEF})
	if err := os.WriteFile(fullPath, bogus, 0o600); err != nil {
		t.Fatalf("write bogus file: %v", err)
	}

	_, err := Open(OpenParams{Root: root, Base: base, DataUUID: uuid, MaxReaders: 8})
	if !errors.Is(err, ErrCorrupted) {
		t.Errorf("got %v, want ErrCorrupted", err)
	}
}

// TestOpenRecoversTornInit pins the crashed-creator staleness class
// (cross-process.md §Lock File Lifecycle): a file left
// partially-initialised by a creator that died without the polite
// unlink — zero-length (crash in the open→flock window) or
// full-size all-zero (crash after Truncate, before the header
// write landed) — is recovered after the adoption budget proves no
// live creator, and Open converges on a freshly-created lock file.
func TestOpenRecoversTornInit(t *testing.T) {
	cases := []struct {
		name    string
		content []byte
	}{
		{"zero-length (open→flock window)", nil},
		{"full-size all-zero (post-truncate)", make([]byte, FileSize(8))},
		// Undersized-with-content is tampering-shaped (no crash
		// produces it), but it is equally unpublishable and
		// unadoptable — the same availability choice as the
		// UUID-zeroing tamper arm applies, and the guard (flock +
		// same-inode-across-the-budget) protects any legitimate
		// slow init strictly better than the former permanent
		// ErrCorrupted did.
		{"undersized non-zero (tampering-shaped)", []byte{0xFF}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root, base, fullPath := tmpLock(t)
			if err := os.WriteFile(fullPath, tc.content, 0o600); err != nil {
				t.Fatalf("write torn file: %v", err)
			}
			f, err := Open(OpenParams{Root: root, Base: base, DataUUID: [16]byte{0xAA}, MaxReaders: 8})
			if err != nil {
				t.Fatalf("Open over torn init: %v", err)
			}
			defer f.Close()
			if got := f.MaxReaders(); got != 8 {
				t.Errorf("recovered MaxReaders = %d, want 8", got)
			}
			st, err := os.Stat(fullPath)
			if err != nil {
				t.Fatalf("stat: %v", err)
			}
			if st.Size() != FileSize(8) {
				t.Errorf("recovered file size = %d, want %d", st.Size(), FileSize(8))
			}
		})
	}
}

// TestTornInitRecoveryDisarmsOnChurn pins the end-to-end contract
// under mid-budget inode churn (someone is making progress on the
// name): ErrCorrupted with the replacement file left intact — never
// a removal. The loop's stability guard is the first line of that
// defense; recoverTornInit's pinned-inode check is the second (a
// neutered stability guard still aborts there and converges to the
// same outcome one budget later — the guard's untestable unique
// value is the A-B-A inode-reuse pin).
func TestTornInitRecoveryDisarmsOnChurn(t *testing.T) {
	root, base, fullPath := tmpLock(t)
	if err := os.WriteFile(fullPath, make([]byte, FileSize(8)), 0o600); err != nil {
		t.Fatal(err)
	}
	// Replace the torn file with a DIFFERENT torn inode partway
	// through Open's ~800 ms budget.
	done := make(chan struct{})
	go func() {
		defer close(done)
		time.Sleep(200 * time.Millisecond)
		_ = os.Remove(fullPath)
		_ = os.WriteFile(fullPath, make([]byte, FileSize(8)), 0o600)
	}()
	_, err := Open(OpenParams{Root: root, Base: base, DataUUID: [16]byte{0xAA}, MaxReaders: 8})
	<-done
	if !errors.Is(err, ErrCorrupted) {
		t.Fatalf("Open under inode churn = %v, want ErrCorrupted (recovery must disarm)", err)
	}
	st, serr := os.Stat(fullPath)
	if serr != nil {
		t.Fatalf("replacement file removed despite churn disarm: %v", serr)
	}
	if st.Size() != FileSize(8) {
		t.Errorf("replacement file altered: size %d", st.Size())
	}
}

// TestOpenAdoptsAfterProgressAbort traverses the progress-abort
// re-run end to end: Open exhausts its budget on a torn file, the
// recovery finds the guard flock CONTENDED (a live peer appeared),
// classifies it progress rather than corruption, re-runs the
// lifecycle, blocks on LOCK_SH until the peer publishes a valid
// header, and adopts it. A fold that treated the progress abort as
// ErrCorrupted would fail here. Timing: the peer takes LOCK_EX at
// ~650 ms — inside the budget's final 256 ms sleep (last adopt
// attempt ≈ 511 ms, exhaustion ≈ 767 ms) — and publishes at ~1 s;
// if scheduling drifts an adopt attempt into the hold, that attempt
// blocks on LOCK_SH and adopts directly, which the assertions also
// accept (the test is timing-stable; only the mutant kill is
// timing-favored).
func TestOpenAdoptsAfterProgressAbort(t *testing.T) {
	root, base, fullPath := tmpLock(t)
	uuid := [16]byte{0xAA}
	if err := os.WriteFile(fullPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	peerDone := make(chan error, 1)
	go func() {
		time.Sleep(650 * time.Millisecond)
		pf, err := os.OpenFile(fullPath, os.O_RDWR, 0)
		if err != nil {
			peerDone <- err
			return
		}
		defer pf.Close()
		if err := flockExclusive(pf.Fd()); err != nil {
			peerDone <- err
			return
		}
		defer func() { _ = flockUnlock(pf.Fd()) }()
		time.Sleep(350 * time.Millisecond)
		peerDone <- initLockFile(pf, uuid, 8, FileSize(8))
	}()

	f, err := Open(OpenParams{Root: root, Base: base, DataUUID: uuid, MaxReaders: 8})
	if err != nil {
		t.Fatalf("Open across a progress-aborted recovery = %v, want adoption of the peer's file", err)
	}
	defer f.Close()
	if perr := <-peerDone; perr != nil {
		t.Fatalf("peer publish: %v", perr)
	}
	if got := f.MaxReaders(); got != 8 {
		t.Errorf("adopted MaxReaders = %d, want 8", got)
	}
	if got := f.UUID(); got != uuid {
		t.Errorf("adopted UUID = %x, want %x", got, uuid)
	}
}

// recoverTornInit guard pins: the recovery must refuse to touch a
// file whose flock is held (a live mid-init creator), whose inode
// was replaced since the stuck observation (a fresh creator's
// open→flock window), or whose header got published meanwhile.
func TestRecoverTornInitGuards(t *testing.T) {
	t.Run("contended flock is a live holder", func(t *testing.T) {
		root, base, fullPath := tmpLock(t)
		if err := os.WriteFile(fullPath, make([]byte, FileSize(8)), 0o600); err != nil {
			t.Fatal(err)
		}
		holder, err := os.OpenFile(fullPath, os.O_RDWR, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer holder.Close()
		if err := flockExclusive(holder.Fd()); err != nil {
			t.Fatalf("witness flock: %v", err)
		}
		defer func() { _ = flockUnlock(holder.Fd()) }()
		want, _ := os.Stat(fullPath)
		if err := recoverTornInit(OpenParams{Root: root, Base: base}, want); err == nil {
			t.Fatal("recovery removed a file whose flock is held")
		} else if !errors.Is(err, errTornProgress) {
			t.Fatalf("contended abort = %v, want errTornProgress (the lifecycle re-runs on it)", err)
		}
		if _, err := os.Stat(fullPath); err != nil {
			t.Fatalf("file removed despite live holder: %v", err)
		}
	})
	t.Run("replaced inode aborts", func(t *testing.T) {
		root, base, fullPath := tmpLock(t)
		if err := os.WriteFile(fullPath, make([]byte, FileSize(8)), 0o600); err != nil {
			t.Fatal(err)
		}
		want, _ := os.Stat(fullPath)
		// A fresh creator replaced the stuck file with its own
		// (also momentarily zero) file.
		if err := os.Remove(fullPath); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(fullPath, make([]byte, FileSize(8)), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := recoverTornInit(OpenParams{Root: root, Base: base}, want); err == nil {
			t.Fatal("recovery removed a replaced (fresh creator's) inode")
		} else if !errors.Is(err, errTornProgress) {
			t.Fatalf("replaced-inode abort = %v, want errTornProgress", err)
		}
		if _, err := os.Stat(fullPath); err != nil {
			t.Fatalf("fresh creator's file removed: %v", err)
		}
	})
	t.Run("published header aborts", func(t *testing.T) {
		root, base, fullPath := tmpLock(t)
		f, err := Open(OpenParams{Root: root, Base: base, DataUUID: [16]byte{0xAA}, MaxReaders: 8})
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		want, _ := os.Stat(fullPath)
		if err := recoverTornInit(OpenParams{Root: root, Base: base}, want); err == nil {
			t.Fatal("recovery removed a finalised lock file")
		} else if !errors.Is(err, errTornProgress) {
			t.Fatalf("published-header abort = %v, want errTornProgress", err)
		}
		if got := f.MaxReaders(); got != 8 {
			t.Errorf("finalised file damaged: MaxReaders = %d", got)
		}
	})
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
	if err := flockShared(holder.Fd()); err != nil {
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
	// Displace the name. Unix removes it; windows cannot remove a
	// mapped file (the kernel gate — cross-process.md WINDOWS PORT
	// DESIGN) but CAN rename it aside, which equally unbinds the name
	// for the identity check.
	if runtime.GOOS == "windows" {
		if err := root.Rename(base, base+".aside"); err != nil {
			t.Fatalf("rename aside: %v", err)
		}
	} else if err := root.Remove(base); err != nil {
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

// Adopting a lock file stamped by a DIFFERENT boot must reset every
// piece of volatile, boot-relative coordination state — pre-boot
// heartbeats read as huge future stamps honoured as fresh forever,
// and PID/starttime identities can collide across boots, so cross-boot
// state would bypass the recovery gate and pin reclamation. Only
// DataGeneration (an inode-replacement counter, not boot-relative)
// survives.
func TestAdoptForeignBootEpochResetsCoordinationState(t *testing.T) {
	if CurrentBootID() == ([16]byte{}) {
		t.Skip("host boot id unreadable: cross-boot invalidation disabled by design")
	}
	root, base, path := tmpLock(t)
	uuid := [16]byte{0xAA}
	f, err := Open(OpenParams{Root: root, Base: base, DataUUID: uuid, MaxReaders: 4})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	// Occupy state a reboot would have orphaned.
	f.SetWriterPID(4242)
	f.SetWriterHeartbeat(999)
	f.SetLastWriterPID(4242)
	f.SetLastWriterHeartbeat(999)
	Store64(&f.Slot(1).TxnID, 33)
	Store64(&f.Slot(1).PID, 4242)
	Store64(&f.Slot(1).Heartbeat, 999)
	f.BumpDataGeneration()
	f.BumpShrinkSeq() // leave it odd, like a writer crashed mid-bracket
	gen := f.DataGeneration()
	f.Close()

	// Forge a foreign boot id in the header (offset of BootID).
	raw, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	foreign := make([]byte, 16)
	foreign[0] = 0xFE
	if _, err := raw.WriteAt(foreign, 112); err != nil {
		t.Fatalf("forge boot id: %v", err)
	}
	raw.Close()

	f2, err := Open(OpenParams{Root: root, Base: base, DataUUID: uuid, MaxReaders: 4})
	if err != nil {
		t.Fatalf("re-adopt: %v", err)
	}
	defer f2.Close()
	if got := f2.WriterPID(); got != 0 {
		t.Errorf("WriterPID = %d, want 0 (cross-boot writer block must reset)", got)
	}
	if tx := Load64(&f2.Slot(1).TxnID); tx != 0 {
		t.Errorf("slot 1 TxnID = %d, want 0 (cross-boot reader slots must reset)", tx)
	}
	if hb := Load64(&f2.Slot(1).Heartbeat); hb != 0 {
		t.Errorf("slot 1 Heartbeat = %d, want 0", hb)
	}
	if s := f2.ShrinkSeq(); s != 0 {
		t.Errorf("ShrinkSeq = %d, want 0 (reset re-evens the seqlock)", s)
	}
	if g := f2.DataGeneration(); g != gen {
		t.Errorf("DataGeneration = %d, want %d (must SURVIVE the reset)", g, gen)
	}
	if lw := f2.LastWriterPID(); lw != 0 {
		t.Errorf("LastWriterPID = %d, want 0 (recovery-gate record must reset)", lw)
	}
}

// A same-boot adoption must NOT reset live coordination state.
func TestAdoptSameBootPreservesCoordinationState(t *testing.T) {
	root, base, _ := tmpLock(t)
	uuid := [16]byte{0xAA}
	f, err := Open(OpenParams{Root: root, Base: base, DataUUID: uuid, MaxReaders: 4})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	Store64(&f.Slot(0).TxnID, 77)
	f.Close()
	f2, err := Open(OpenParams{Root: root, Base: base, DataUUID: uuid, MaxReaders: 4})
	if err != nil {
		t.Fatalf("re-adopt: %v", err)
	}
	defer f2.Close()
	if tx := Load64(&f2.Slot(0).TxnID); tx != 77 {
		t.Errorf("slot 0 TxnID = %d, want 77 (same-boot state must survive)", tx)
	}
}

// A zero boot id on EITHER side must disable the cross-boot reset:
// resetting on an unknown epoch could evict a live same-boot peer's
// coordination state (use-after-reclaim) — strictly worse than the
// cross-boot staleness the reset exists to fix.
func TestZeroBootEpochNeverResets(t *testing.T) {
	var zero, a, b [16]byte
	a[0], b[0] = 1, 2
	for name, c := range map[string]struct {
		stamped, current [16]byte
		want             bool
	}{
		"both zero":           {zero, zero, false},
		"stamped zero":        {zero, a, false},
		"current zero":        {a, zero, false},
		"known and equal":     {a, a, false},
		"known and different": {a, b, true},
	} {
		if got := shouldResetBootEpoch(c.stamped, c.current); got != c.want {
			t.Errorf("%s: shouldResetBootEpoch = %v, want %v", name, got, c.want)
		}
	}
}

// A contended boot-epoch reset (another holder on the file's flock)
// backs off and retries; with the holder never releasing, Open
// exhausts its budget with the contended sentinel and the foreign
// state survives untouched.
func TestBootEpochResetContendedSkips(t *testing.T) {
	if CurrentBootID() == ([16]byte{}) {
		t.Skip("host boot id unreadable: cross-boot invalidation disabled by design")
	}
	root, base, path := tmpLock(t)
	uuid := [16]byte{0xAA}
	f, err := Open(OpenParams{Root: root, Base: base, DataUUID: uuid, MaxReaders: 4})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	Store64(&f.Slot(0).TxnID, 33)
	f.Close()
	// Forge a foreign (non-zero) boot id.
	raw, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	forged := make([]byte, 16)
	forged[0] = 0xFE
	if _, err := raw.WriteAt(forged, 112); err != nil {
		t.Fatalf("forge: %v", err)
	}
	raw.Close()
	// A live flock holder blocks the LOCK_EX conversion.
	holder, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("holder: %v", err)
	}
	defer holder.Close()
	if err := flockShared(holder.Fd()); err != nil {
		t.Fatalf("holder flock: %v", err)
	}

	_, err = Open(OpenParams{Root: root, Base: base, DataUUID: uuid, MaxReaders: 4})
	if err == nil {
		t.Fatal("Open succeeded despite a contended boot-epoch reset")
	}
	if !errors.Is(err, errBootEpochContended) {
		t.Fatalf("budget exhausted by %v, want errBootEpochContended", err)
	}
	// The foreign state must be untouched (no unguarded reset).
	f2raw, err := os.OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		t.Fatalf("verify open: %v", err)
	}
	defer f2raw.Close()
	buf := make([]byte, 8)
	if _, err := f2raw.ReadAt(buf, HeaderSize); err != nil { // slot 0 TxnID
		t.Fatalf("read slot: %v", err)
	}
	if v := le64(buf); v != 33 {
		t.Fatalf("slot 0 TxnID = %d, want 33 (state reset without the lock)", v)
	}
}

func le64(b []byte) uint64 {
	var v uint64
	for i := 7; i >= 0; i-- {
		v = v<<8 | uint64(b[i])
	}
	return v
}

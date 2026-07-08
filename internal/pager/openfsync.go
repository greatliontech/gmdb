package pager

import (
	"os"
	"sync/atomic"
)

// openFsync is the gated writable Open's fdatasync seam: the anchor
// rewrite's fsync ("anchor") and the recovery commit's fsync
// ("recovery-commit") route through it so tests can inject failures on
// exactly these two durability points (durability.md §Anchoring /
// §Recovery step 5). Production behavior is fdatasync verbatim.
//
// The anchor REWRITE itself (the byte-identical meta pwrite preceding
// the "anchor" fsync) has no DATA-observable distinguisher — dropping
// it changes only kernel dirty-page state — but it IS pinned via a
// syscall-side-effect proxy: POSIX mandates write(2) marks st_mtime
// for update while fdatasync does not, so a test that Chtimes the
// file to a sentinel epoch and stats it after the arm proves the
// write occurred (the mtime-sentinel pin in this file's test twin).
//
// Hook placement is deliberately BEFORE the real fdatasync (an
// injected failure skips it — modeling "fsync failed, nothing
// flushed", the conservative worst case). This is the OPPOSITE of the
// checkpoint step hook, which fires after its syscall succeeded — the
// right shape for poison-semantics tests. Both are correct for their
// consumers; do not unify them.
func openFsync(f *os.File, op string) error {
	if hook := openFsyncHookForTest.Load(); hook != nil {
		if err := (*hook)(op); err != nil {
			return err
		}
	}
	return fdatasync(f)
}

var openFsyncHookForTest atomic.Pointer[func(op string) error]

// SetOpenFsyncHookForTest injects an error before the gated Open's
// fsyncs (op = "anchor" | "recovery-commit"; nil return = proceed).
// Returns a restore func. Test-only.
func SetOpenFsyncHookForTest(hook func(op string) error) (restore func()) {
	if hook == nil {
		openFsyncHookForTest.Store(nil)
		return func() {}
	}
	openFsyncHookForTest.Store(&hook)
	return func() { openFsyncHookForTest.Store(nil) }
}

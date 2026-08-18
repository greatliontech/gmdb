package oslock

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestTryAcquireVerdicts pins the verdict semantics: an unheld path
// acquires (creating the file); a held path answers ErrHeld; two
// descriptors in one process exclude each other exactly as two
// processes do (this same shape is the startup soundness probe the
// package doc prescribes); release frees the next acquirer.
func TestTryAcquireVerdicts(t *testing.T) {
	p := filepath.Join(t.TempDir(), "claim")
	a, err := TryAcquire(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := TryAcquire(p); !errors.Is(err, ErrHeld) {
		t.Fatalf("second acquisition: %v, want ErrHeld", err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	b, err := TryAcquire(p)
	if err != nil {
		t.Fatalf("after release: %v", err)
	}
	b.Close()
}

// TestAcquireWaitsForRelease pins the blocking arm: a waiter parks
// on a held lock and proceeds when the holder releases.
func TestAcquireWaitsForRelease(t *testing.T) {
	p := filepath.Join(t.TempDir(), "claim")
	a, err := TryAcquire(p)
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan error, 1)
	go func() {
		b, err := Acquire(context.Background(), p)
		if err == nil {
			b.Close()
		}
		got <- err
	}()
	select {
	case err := <-got:
		t.Fatalf("waiter proceeded against a held lock: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	a.Close()
	select {
	case err := <-got:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waiter never proceeded after release")
	}
}

// TestAcquireCancel pins cancellability: a waiter on a held lock
// returns the context's error, the holder's lock survives the
// abandoned wait, and — the poll design's point — the cancelled
// wait leaves nothing behind that could ever take the lock: once
// the holder releases, a fresh acquirer proceeds.
func TestAcquireCancel(t *testing.T) {
	p := filepath.Join(t.TempDir(), "claim")
	a, err := TryAcquire(p)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := Acquire(ctx, p); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancelled wait: %v", err)
	}
	// The holder still holds: a fresh verdict reads live.
	if _, err := TryAcquire(p); !errors.Is(err, ErrHeld) {
		t.Fatalf("holder lost the lock to a cancelled waiter: %v", err)
	}
	a.Close()
	// Nothing from the cancelled wait can claim the freed lock.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	b, err := Acquire(ctx2, p)
	if err != nil {
		t.Fatalf("cancelled wait left a claimant behind: %v", err)
	}
	b.Close()
}

// TestVerifiedCatchesUnlinkRecreate pins the identity comparison: a
// descriptor whose file was unlinked and recreated underfoot no
// longer matches the path.
func TestVerifiedCatchesUnlinkRecreate(t *testing.T) {
	p := filepath.Join(t.TempDir(), "claim")
	f, err := open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if !verified(f, p) {
		t.Fatal("fresh descriptor failed identity verify")
	}
	if err := os.Remove(p); err != nil {
		t.Skipf("platform refuses unlink of open files: %v", err)
	}
	if verified(f, p) {
		t.Fatal("unlinked descriptor passed identity verify")
	}
	g, err := open(p)
	if err != nil {
		// Windows: the unlinked file sits delete-pending while f is
		// open and the name cannot be reused yet — the recreate arm
		// is unreachable there.
		t.Skipf("platform refuses recreate while descriptor open: %v", err)
	}
	defer g.Close()
	if verified(f, p) {
		t.Fatal("descriptor matched a recreated file")
	}
	if !verified(g, p) {
		t.Fatal("descriptor on the recreated file failed identity verify")
	}
}

// TestAcquireRetriesUnlinkRecreateRace drives the unlink-recreate
// race through the acquisition loops themselves (the seam runs
// between lock and verify): the returned Lock must hold the path's
// CURRENT file, reached by retrying — never the retired inode.
func TestAcquireRetriesUnlinkRecreateRace(t *testing.T) {
	for name, acquire := range map[string]func(string) (*Lock, error){
		"TryAcquire": TryAcquire,
		"Acquire": func(p string) (*Lock, error) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			return Acquire(ctx, p)
		},
	} {
		t.Run(name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "claim")
			// The hook runs inside library code holding a locked
			// descriptor: it must never unwind the test through it
			// (t.Fatal/t.Skip would leak the locked fd), so it only
			// records and the assertions run after acquire returns.
			calls := 0
			var skipReason string
			restore := setAfterLockHookForTest(func(path string) {
				calls++
				if calls != 1 {
					return
				}
				// The race: the locked file is unlinked and the path
				// recreated before the verify looks.
				if err := os.Remove(path); err != nil {
					skipReason = fmt.Sprintf("platform refuses unlink of open files: %v", err)
					return
				}
				g, err := open(path)
				if err != nil {
					// Windows delete-pending: the name is unusable
					// while the locked descriptor is open.
					skipReason = fmt.Sprintf("platform refuses recreate while descriptor open: %v", err)
					return
				}
				g.Close()
			})
			defer restore()
			l, err := acquire(p)
			if skipReason != "" {
				if err == nil {
					l.Close()
				}
				t.Skip(skipReason)
			}
			if err != nil {
				t.Fatal(err)
			}
			defer l.Close()
			if calls < 2 {
				t.Fatalf("acquisition kept the retired inode without retrying (verify ran %d time)", calls)
			}
			if !verified(l.f, p) {
				t.Fatal("returned Lock does not hold the path's current file")
			}
		})
	}
}

// TestChurnedPathNeverWedges pins the verify-retry gates against a
// path churned on every acquisition (the hook unlinks and recreates
// each time): Acquire stays cancellable and returns the context's
// cause; TryAcquire exhausts its bounded budget and returns an
// undecided error — neither spins forever, neither reads death.
func TestChurnedPathNeverWedges(t *testing.T) {
	// bounded runs fn with a hard deadline so a wedged acquisition
	// (a broken gate spinning forever) fails this test by name
	// instead of hanging into the package timeout's goroutine dump.
	bounded := func(t *testing.T, fn func() (*Lock, error)) (*Lock, error) {
		t.Helper()
		type res struct {
			l   *Lock
			err error
		}
		ch := make(chan res, 1)
		go func() {
			l, err := fn()
			ch <- res{l, err}
		}()
		select {
		case r := <-ch:
			return r.l, r.err
		case <-time.After(10 * time.Second):
			t.Fatal("acquisition wedged on a churned path")
			return nil, nil
		}
	}
	newChurn := func(calls *int) func(string) {
		return func(path string) {
			*calls++
			if err := os.Remove(path); err != nil {
				return
			}
			if g, err := open(path); err == nil {
				g.Close()
			}
		}
	}

	t.Run("Acquire", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "claim")
		calls := 0
		restore := setAfterLockHookForTest(newChurn(&calls))
		defer restore()
		errMine := errors.New("caller gave up")
		ctx, cancel := context.WithCancelCause(context.Background())
		go func() {
			time.Sleep(100 * time.Millisecond)
			cancel(errMine)
		}()
		start := time.Now()
		l, err := bounded(t, func() (*Lock, error) { return Acquire(ctx, p) })
		elapsed := time.Since(start)
		if err == nil {
			l.Close()
			t.Skip("platform defers unlink of open files; churn unreachable")
		}
		if !errors.Is(err, errMine) {
			t.Fatalf("churned Acquire: %v, want the cancellation cause", err)
		}
		// Pacing oracle, measured against the wait's real duration
		// so a delayed cancel wakeup cannot flake it: paced churn
		// probes about once per poll interval; an unpaced spin
		// probes thousands of times in the same window.
		if max := int(elapsed/acquirePollInterval) + 20; calls > max {
			t.Fatalf("churn not paced: %d probes in %v (max %d)", calls, elapsed, max)
		}
	})

	t.Run("TryAcquire", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "claim")
		calls := 0
		restore := setAfterLockHookForTest(newChurn(&calls))
		defer restore()
		l, err := bounded(t, func() (*Lock, error) { return TryAcquire(p) })
		if err == nil {
			l.Close()
			t.Skip("platform defers unlink of open files; churn unreachable")
		}
		if !errors.Is(err, ErrUndecided) || errors.Is(err, ErrHeld) {
			t.Fatalf("churn exhaustion: %v, want ErrUndecided", err)
		}
		// The budget is a real tolerance for benign races, not a
		// hair trigger: the spec names its order (~a hundred), and
		// under sustained churn the count is deterministic
		// (budget+1 hook calls), so the floor can pin the order.
		if calls <= 50 {
			t.Fatalf("budget exhausted after %d retries — spec claims the order of a hundred", calls)
		}
	})
}

// TestRetire pins the retirement discipline: Retire unlinks while
// held then releases, the successor claims a fresh file, and Retire
// after Close is a refused no-op — a closed Lock can never unlink a
// successor's live lock file.
func TestRetire(t *testing.T) {
	p := filepath.Join(t.TempDir(), "claim")
	a, err := TryAcquire(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Retire(); err != nil {
		if errors.Is(err, ErrUnlinkDeferred) {
			t.Skipf("platform defers unlink of open files: %v", err)
		}
		t.Fatal(err)
	}
	b, err := TryAcquire(p)
	if err != nil {
		t.Fatalf("successor blocked after retirement: %v", err)
	}
	// A retired (closed) Lock must never unlink the successor's
	// live lock file.
	if err := a.Retire(); !errors.Is(err, ErrRetired) {
		t.Fatalf("retire after close: %v, want ErrRetired", err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("successor's lock file gone: %v", err)
	}
	b.Close()
}

// TestCrossProcessVerdicts is the package's central claim: a lock
// held by another process answers ErrHeld, and the kernel releases
// it at that process's death — SIGKILL included — after which
// acquisition succeeds. The child execs this test binary,
// env-gated.
func TestCrossProcessVerdicts(t *testing.T) {
	p := os.Getenv("GMDB_OSLOCK_TEST_CHILD_PATH")
	if p != "" {
		l, err := TryAcquire(p)
		if err != nil {
			fmt.Println("child:", err)
			os.Exit(1)
		}
		defer l.Close()
		fmt.Println("held")
		select {} // hold until killed
	}

	p = filepath.Join(t.TempDir(), "claim")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run", "^TestCrossProcessVerdicts$")
	cmd.Env = append(os.Environ(), "GMDB_OSLOCK_TEST_CHILD_PATH="+p)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cmd.Process.Kill()
		cmd.Wait()
	}()
	// The child's stdout carries the test runner's own lines before
	// the child's "held" — scan, never single-read.
	held := make(chan bool, 1)
	go func() {
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			if strings.Contains(sc.Text(), "held") {
				held <- true
				return
			}
		}
		held <- false
	}()
	select {
	case ok := <-held:
		if !ok {
			t.Fatal("child never acquired")
		}
	case <-time.After(30 * time.Second):
		// Kill first and drain the scanner before failing: the
		// deferred Wait closes the pipe, which must not race a
		// still-reading scanner.
		cmd.Process.Kill()
		<-held
		t.Fatal("child never reported")
	}

	if _, err := TryAcquire(p); !errors.Is(err, ErrHeld) {
		t.Fatalf("live child's lock judged: %v, want ErrHeld", err)
	}

	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	cmd.Wait()

	// The kernel released with the child; the claim is acquirable.
	// Windows releases the range lock at handle teardown, which can
	// trail process exit by a moment — bound the wait instead of
	// asserting instantly.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	l, err := Acquire(ctx, p)
	if err != nil {
		t.Fatalf("dead child's lock not released: %v", err)
	}
	l.Close()
}

// TestAcquireSurfacesPersistentOpenFailure pins the bounded open
// budget: a path that can never open (missing parent) returns its
// error rather than polling until the context dies.
func TestAcquireSurfacesPersistentOpenFailure(t *testing.T) {
	p := filepath.Join(t.TempDir(), "absent-dir", "claim")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := Acquire(ctx, p); err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("persistent open failure: %v, want the open error", err)
	}

	// Cancellation inside the open-retry wait carries the CAUSE,
	// exactly like cancellation inside the lock poll — the two arms
	// are distinct code paths and each needs its own pin.
	errMine := errors.New("caller gave up")
	ctx2, cancel2 := context.WithCancelCause(context.Background())
	go func() {
		time.Sleep(5 * time.Millisecond)
		cancel2(errMine)
	}()
	if _, err := Acquire(ctx2, p); !errors.Is(err, errMine) {
		t.Fatalf("open-retry cancellation: %v, want the cancellation cause", err)
	}
}

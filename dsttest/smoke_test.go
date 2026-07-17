//go:build dst

package dsttest

import (
	"context"
	"os"
	"testing"
	"testing/simulation"
	"time"

	"github.com/greatliontech/gmdb"
	"github.com/greatliontech/gmdb/internal/lock"
)

// The fence probe (docs/specs/dst-testing.md §Simulated syscall
// surface, INV: no fenced syscall reachable): gmdb's full production
// path — open (flock, mmap, madvise), write+commit (pwrite,
// fdatasync, directory fsync), a genuinely BLOCKING change
// notification wait (the real shared FUTEX_WAIT, woken by a
// concurrent commit's FUTEX_WAKE), CopyTo publish (the simulated
// filesystem models link(2), so this exercises the preferred
// hard-link arm; the renameat2 NOREPLACE rung is unreachable through
// CopyTo in-simulation — a modeled FS supports hard links — and its
// coverage lives in the untagged unit tier's publish-seam tests and
// the fork's own raw-dispatch tests, per the spec's §Simulated
// syscall surface), close, reopen, Check, read-back — runs
// fence-free, identically across the anchor seeds. Identity pins:
// the simulated /proc surface serves the pid start-time and
// pid-namespace discriminators the stale detection keys on (db.go
// tolerates their absence silently, so without this pin a fork
// regression would degrade the coordination-suite legs to the wrong
// path).
func TestSimulationFullSurfaceFenceFree(t *testing.T) {
	for _, seed := range []uint64{1, 2, 3} { // anchor seeds (spec §Seed policy)
		simulation.Test(t, seed, func(t *testing.T) {
			ctx := context.Background()
			if _, err := lock.ProcessStartTime(os.Getpid()); err != nil {
				t.Fatalf("seed %d: ProcessStartTime: %v", seed, err)
			}
			if _, err := lock.PIDNamespace(); err != nil {
				t.Fatalf("seed %d: PIDNamespace: %v", seed, err)
			}
			// link(2)-modeled tripwire: CopyTo's preferred hard-link arm is
			// only exercised if the simulated filesystem models link — and
			// CopyTo tolerates link absence silently (the rename fallback),
			// so the spec's link-arm claim needs its own pin, exactly like
			// the identity pins below.
			if err := os.WriteFile("/link-probe", []byte("x"), 0o600); err != nil {
				t.Fatalf("seed %d: link probe write: %v", seed, err)
			}
			if err := os.Link("/link-probe", "/link-probe2"); err != nil {
				t.Fatalf("seed %d: os.Link under simulation = %v, want modeled (CopyTo's hard-link arm depends on it)", seed, err)
			}
			fi1, err1 := os.Stat("/link-probe")
			fi2, err2 := os.Stat("/link-probe2")
			if err1 != nil || err2 != nil || !os.SameFile(fi1, fi2) {
				t.Fatalf("seed %d: linked names not one inode (%v/%v)", seed, err1, err2)
			}
			db, err := gmdb.Open(ctx, "/smoke.db", gmdb.Options{PageSize: 4096, MinSize: 16, MaxSize: 256})
			if err != nil {
				t.Fatalf("seed %d: Open: %v", seed, err)
			}
			put := func(k, v string) {
				if err := db.Update(ctx, func(tx *gmdb.Tx) error {
					ks, e := tx.CreateKeyspace("k")
					if e != nil {
						return e
					}
					return ks.Put([]byte(k), []byte(v))
				}); err != nil {
					t.Fatalf("seed %d: Update: %v", seed, err)
				}
			}
			put("a", "b")
			// A genuinely blocking wait: the current version cannot move
			// until the concurrent commit publishes, so the waiter parks
			// in the real FUTEX_WAIT slice and the publish's FUTEX_WAKE
			// releases it.
			v, err := db.Version()
			if err != nil {
				t.Fatalf("seed %d: Version: %v", seed, err)
			}
			go func() {
				time.Sleep(20 * time.Millisecond) // virtual: waiter parks first
				if err := db.Update(ctx, func(tx *gmdb.Tx) error {
					ks, e := tx.OpenKeyspace("k")
					if e != nil {
						return e
					}
					return ks.Put([]byte("a2"), []byte("b2"))
				}); err != nil {
					t.Errorf("seed %d: concurrent Update: %v", seed, err)
				}
			}()
			waitStart := time.Now()
			if got, err := db.WaitVersion(ctx, v); err != nil || got <= v {
				t.Fatalf("seed %d: WaitVersion(%d) = (%d, %v)", seed, v, got, err)
			}
			// Woken well inside the first 100ms wait slice: the publish's
			// FUTEX_WAKE released the waiter — not the slice-timeout
			// recheck, which would mask a lost wake (virtual clock, so
			// the bound is exact per seed).
			if el := time.Since(waitStart); el >= 100*time.Millisecond {
				t.Fatalf("seed %d: WaitVersion took %v of virtual time — slice-timeout recheck, not the wake", seed, el)
			}
			if err := db.CopyTo("/smoke-copy.db", false); err != nil {
				t.Fatalf("seed %d: CopyTo: %v", seed, err)
			}
			if err := db.Close(); err != nil {
				t.Fatalf("seed %d: Close: %v", seed, err)
			}
			db2, err := gmdb.Open(ctx, "/smoke.db", gmdb.Options{PageSize: 4096, MinSize: 16, MaxSize: 256})
			if err != nil {
				t.Fatalf("seed %d: re-Open: %v", seed, err)
			}
			defer db2.Close()
			for issue := range db2.Check() {
				t.Errorf("seed %d: Check: %+v", seed, issue)
			}
			if err := db2.View(ctx, func(rtx *gmdb.ReadTx) error {
				ks, e := rtx.OpenKeyspaceReadOnly("k")
				if e != nil {
					return e
				}
				v, e := ks.Get([]byte("a"))
				if e != nil {
					return e
				}
				if string(v) != "b" {
					t.Errorf("seed %d: Get = %q, want b", seed, v)
				}
				return nil
			}); err != nil {
				t.Fatalf("seed %d: View: %v", seed, err)
			}
		})
	}
}

// The boot-epoch surface (spec §Simulated syscall surface): the fork
// models /proc/sys/kernel/random/boot_id, so gmdb reads a REAL
// per-boot epoch in-simulation — nonzero, stable within a boot, and
// distinct across seeds (each run is a different universe). Cross-boot
// invalidation is therefore ACTIVE in-simulation; the reset behavior
// itself is the boot-epoch suite's job (bootepoch_test.go). A
// regression to the zero epoch would silently degrade that suite to
// the invalidation-disabled path — this pin fails it loudly instead.
func TestSimulationBootEpochModeled(t *testing.T) {
	var id1, id1Again, id2 [16]byte
	simulation.Test(t, 1, func(t *testing.T) {
		id1 = lock.CurrentBootID()
		id1Again = lock.CurrentBootID()
	})
	simulation.Test(t, 2, func(t *testing.T) {
		id2 = lock.CurrentBootID()
	})
	if id1 == ([16]byte{}) || id1 != id1Again {
		t.Fatalf("boot id under simulation = %x then %x, want stable nonzero", id1, id1Again)
	}
	if id2 == id1 || id2 == ([16]byte{}) {
		t.Fatalf("seed-2 boot id = %x, want nonzero and distinct from seed 1's %x", id2, id1)
	}
}

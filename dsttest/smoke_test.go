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
// concurrent commit's FUTEX_WAKE), CopyTo publish (in-simulation the
// link(2) arm is unmodeled, so this always exercises the real
// renameat2 NOREPLACE rung — the spec's recorded residual), close,
// reopen, Check, read-back — runs fence-free, identically across the
// anchor seeds. Identity pins: the simulated /proc surface serves
// the pid start-time and pid-namespace discriminators the stale
// detection keys on (db.go tolerates their absence silently, so
// without this pin a fork regression would degrade chunk-2 legs to
// the wrong path).
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

// The recorded boot-epoch residual (spec §Simulated syscall surface):
// /proc/sys/kernel/random/boot_id is unmodeled, the read fails, and
// gmdb runs with the ZERO boot epoch — cross-boot invalidation
// disabled, exactly its spec'd degradation for unreadable-/proc
// environments. This pin turns the residual from an assumption into
// an enforced fact; if the fork ever models boot_id, this test fails
// and the boot-epoch suite's Lands: condition has fired.
//
// Order caveat: CurrentBootID caches per OS process (sync.Once). The
// pin holds because every caller in this dst test binary runs inside
// a simulation bubble (the simulated read fails → zero cached). A
// future dst test touching gmdb or internal/lock OUTSIDE a bubble
// before these tests would cache the HOST boot id and fail this test
// for an unrelated reason — keep dsttest bubble-only.
func TestSimulationBootEpochIsZero(t *testing.T) {
	simulation.Test(t, 1, func(t *testing.T) {
		if id := lock.CurrentBootID(); id != [16]byte{} {
			t.Fatalf("boot id under simulation = %x, want zero (unmodeled /proc/sys)", id)
		}
	})
}

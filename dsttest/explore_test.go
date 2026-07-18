//go:build dst

package dsttest

import (
	"context"
	"os"
	"runtime"
	"sync/atomic"
	"testing"
	"testing/simulation"
	"time"

	"github.com/greatliontech/gmdb"
)

// Exploration tier (docs/specs/dst-testing.md §Exploration tier +
// §Seed policy): DPOR/exhaustive interleaving exploration over the
// hottest gmdb surfaces — commit vs concurrent readers, commit vs the
// notification waiter, commit vs the maintenance daemon — plus the
// Replay workflow proven end-to-end on a known-buggy SUT, and a PCT
// leg over the same reader surface. Every failure report carries the
// seed and the replayable schedule; exploration budgets are reported,
// never silently capped.

// exploreReport asserts an exploration found no failures, REPLAYS any
// failure it did find to confirm determinism, and reports coverage
// honestly (BudgetHit/Overflow/ForeignSched are stated, not hidden).
func exploreReport(t *testing.T, tag string, seed uint64, res simulation.ExploreResult, sut func() bool) {
	t.Helper()
	t.Logf("%s: seed %d: mode=%v %d schedules, exhausted=%v budgetHit=%v overflow=%v foreign=%v uninstrumented=%v",
		tag, seed, exploreMode(), res.Schedules, res.Exhausted, res.BudgetHit, res.Overflow, res.ForeignSched, res.Uninstrumented)
	replay := func(f simulation.Failure) (failed bool) {
		if f.Panic != "" || f.Deadlock != "" {
			// Replay reproduces panic/deadlock failures by PANICKING with
			// the same marker (the fork's contract) — reproduction is the
			// recovered panic, not the return value.
			defer func() { failed = recover() != nil }()
		}
		failed, _ = simulation.Replay(seed, f, sut)
		return failed
	}
	for _, f := range res.Failures {
		failed := replay(f)
		if !failed && f.ForeignSched {
			// A non-reproducing replay of a foreign-sched-tainted failure
			// is the fork's documented best-effort bound, not a
			// determinism bug — say which it is.
			t.Errorf("%s: seed %d: failure schedule=%v did NOT replay (ForeignSched-tainted; best-effort per the fork's replay contract)",
				tag, seed, f.Schedule)
			continue
		}
		t.Errorf("%s: seed %d: failure (race=%v panic=%q deadlock=%q) schedule=%v — Replay reproduces: %v",
			tag, seed, f.Race, f.Panic, f.Deadlock, f.Schedule, failed)
	}
}

// TestSimulationExploreReplayWorkflow proves the Explore → Failure →
// Replay workflow end-to-end on a SUT with a KNOWN lost-update bug, so
// the suite's failure path is demonstrated working before a real gmdb
// bug ever needs it: exploration finds the buggy interleaving, and
// Replay deterministically reproduces exactly it.
func TestSimulationExploreReplayWorkflow(t *testing.T) {
	// The known bug is an ATOMIC lost update: TSan-clean (a plain-memory
	// race would fail the -race test binary at the process level before
	// the explorer could own it), yet a genuine atomicity violation both
	// engines explore — non-race Exhaustive through the Gosched yield,
	// dst-race DPOR through the instrumented atomic ops.
	var x atomic.Int64
	sut := func() bool {
		x.Store(0)
		done := make(chan struct{}, 2)
		for range 2 {
			go func() {
				v := x.Load()
				runtime.Gosched() // the lost-update window
				x.Store(v + 1)
				done <- struct{}{}
			}()
		}
		<-done
		<-done
		return x.Load() != 2 // true = the bug manifested
	}
	// Non-race: exhaustive over the yield-granularity tree, exhausted.
	// dst-race: the same bug is ALSO a TSan race, access instrumentation
	// multiplies the decision points, and DPOR prunes — bound the run
	// and assert find+replay, not exhaustion.
	opts := simulation.ExploreOptions{Mode: simulation.Exhaustive}
	if raceEnabled {
		opts = simulation.ExploreOptions{Mode: simulation.DPOR, MaxSchedules: 200}
	}
	res := simulation.ExploreWith(1, opts, sut)
	if len(res.Failures) == 0 {
		t.Fatalf("exploration of a known lost-update bug found no failure in %d schedules", res.Schedules)
	}
	asserted := 0
	for _, f := range res.Failures {
		failed, raced := simulation.Replay(1, f, sut)
		if f.Race {
			// TSan dedups race reports process-wide, so an in-process
			// replay of a Race failure cannot re-report it (the fork's
			// replay token targets fresh-process replay); exercising
			// Replay without asserting reproduction is all this build
			// can pin for the race-typed entries.
			_ = raced
			continue
		}
		asserted++
		if !failed {
			t.Fatalf("Replay(schedule %v) did not reproduce the recorded assertion failure", f.Schedule)
		}
	}
	if asserted == 0 {
		t.Fatalf("no assertion-typed failure to replay-verify among %d failures", len(res.Failures))
	}
	if !raceEnabled && !res.Exhausted {
		t.Fatalf("tiny SUT not exhausted: %+v", res)
	}
}

// exploreOpts is the budgeted configuration the gmdb legs share.
// Exhaustive, not DPOR: this suite builds without -race, where the
// fork's DPOR sees no dependency events for gmdb's conflict channels
// (mmap'd lock header, simulated FS, flock) and honestly reports
// Uninstrumented instead of exploring (see
// TestSimulationExploreDPORRequiresRaceBuild); Exhaustive enumerates
// the yield-granularity schedule tree, which is real interleaving
// diversity in any build. gmdb's coordination machinery makes that
// tree unbounded for practical purposes, so each leg explores a
// capped schedule set per seed and REPORTS the cap (no silent
// truncation). A dst-race build may flip this to DPOR for
// dependency-pruned coverage.
var exploreOpts = simulation.ExploreOptions{Mode: exploreMode(), MaxSchedules: 250}

// exploreMode is DPOR in every build: the fork's coarse cross-process
// dependency model (file nodes, the host namespace, flock, the shared
// futex) is build-independent, so non-race DPOR genuinely explores and
// prunes gmdb's multi-process conflicts at OS-object granularity; the
// dst-race build (the Taskfile's test:dst:race leg) adds
// memory-granularity dependencies on top, at full-stack budgets that
// overflow after one schedule (the recorded scale boundary).
func exploreMode() simulation.ExploreMode {
	return simulation.DPOR
}

// TestSimulationExploreDPORCoarseVisibility pins that gmdb's
// cross-process conflicts are VISIBLE to non-race DPOR through the
// fork's coarse dependency model (file nodes, namespace, flock, futex):
// the exploration must never report Uninstrumented — gmdb SUTs fire
// coarse events by construction — and any failure it finds must replay.
// (Before the coarse model, non-race DPOR honestly reported
// Uninstrumented here; that downgrade now belongs to SUTs with NO
// OS-visible conflicts, pinned in the fork's own suite.)
func TestSimulationExploreDPORCoarseVisibility(t *testing.T) {
	sut := func() bool {
		simulation.Host("h", simulation.HostConfig{}, func() {
			simulation.Process("setup", func() {
				ctx := context.Background()
				db := openDB(t, "/db")
				defer db.Close()
				if err := db.Update(ctx, func(tx *gmdb.Tx) error {
					ks, e := tx.CreateKeyspace("k")
					if e != nil {
						return e
					}
					return ks.Put([]byte("a"), []byte("v"))
				}); err != nil {
					t.Fatalf("setup: %v", err)
				}
			})
			done := make(chan struct{}, 2)
			writer := func(v string) {
				defer func() { done <- struct{}{} }() // after Close (LIFO): no closers parked at bubble exit
				ctx := context.Background()
				db := openDB(t, "/db")
				defer db.Close()
				db.Update(ctx, func(tx *gmdb.Tx) error {
					ks, e := tx.OpenKeyspace("k")
					if e != nil {
						return e
					}
					return ks.Put([]byte("a"), []byte(v))
				})
			}
			go simulation.Process("w1", func() { writer("w1") })
			go simulation.Process("w2", func() { writer("w2") })
			<-done
			<-done
		})
		return false
	}
	res := simulation.ExploreWith(60, simulation.ExploreOptions{Mode: simulation.DPOR, MaxSchedules: 50}, sut)
	t.Logf("dpor: schedules=%d exhausted=%v uninstrumented=%v", res.Schedules, res.Exhausted, res.Uninstrumented)
	if res.Uninstrumented {
		t.Fatalf("gmdb SUT reported Uninstrumented — the coarse dependency model saw none of its OS conflicts: %+v", res)
	}
	for _, f := range res.Failures {
		failed, _ := simulation.Replay(60, f, sut)
		t.Errorf("dpor: failure schedule=%v — Replay reproduces: %v", f.Schedule, failed)
	}
}

// TestSimulationExploreCommitVsReaders explores commit / reader
// interleavings across two processes: whatever the schedule, a reader
// observes exactly the old or the new value of the key — never a torn
// or missing state (cross-process.md reader-snapshot contract).
func TestSimulationExploreCommitVsReaders(t *testing.T) {
	for _, seed := range sweepSeeds(t, 61, 62) {
		sut := func() bool {
			violation := false
			simulation.Host("h", simulation.HostConfig{}, func() {
				simulation.Process("setup", func() {
					ctx := context.Background()
					db := openDB(t, "/db")
					defer db.Close()
					if err := db.Update(ctx, func(tx *gmdb.Tx) error {
						ks, e := tx.CreateKeyspace("k")
						if e != nil {
							return e
						}
						return ks.Put([]byte("a"), []byte("old"))
					}); err != nil {
						t.Fatalf("setup: %v", err)
					}
				})
				// Both handles open BEFORE the race and rendezvous on a go
				// signal: the explored decision budget then concentrates on
				// the commit-vs-read window itself instead of being spent
				// reordering the (uninteresting) Open machinery — without
				// the rendezvous, a budget-bounded sweep never reaches a
				// post-commit read (mutation-verified).
				writerDone := make(chan struct{})
				go simulation.Process("writer", func() {
					defer close(writerDone) // after Close: the join below must not leave closers parked at bubble exit
					ctx := context.Background()
					db := openDB(t, "/db")
					defer db.Close()
					if err := os.WriteFile("/writer-ready", nil, 0o600); err != nil {
						t.Errorf("signal: %v", err)
						return
					}
					mustAwait(t, "/go")
					if err := db.Update(ctx, func(tx *gmdb.Tx) error {
						ks, e := tx.OpenKeyspace("k")
						if e != nil {
							return e
						}
						return ks.Put([]byte("a"), []byte("new"))
					}); err != nil {
						t.Errorf("writer: %v", err)
					}
				})
				simulation.Process("reader", func() {
					ctx := context.Background()
					db := openDB(t, "/db")
					defer db.Close()
					mustAwait(t, "/writer-ready")
					if err := os.WriteFile("/go", nil, 0o600); err != nil {
						t.Errorf("signal: %v", err)
						return
					}
					if err := db.View(ctx, func(rtx *gmdb.ReadTx) error {
						ks, e := rtx.OpenKeyspaceReadOnly("k")
						if e != nil {
							return e
						}
						v, e := ks.Get([]byte("a"))
						if e != nil || (string(v) != "old" && string(v) != "new") {
							violation = true
						}
						return nil
					}); err != nil {
						violation = true
					}
				})
				<-writerDone
			})
			return violation
		}
		res := simulation.ExploreWith(seed, exploreOpts, sut)
		exploreReport(t, "commit-vs-readers", seed, res, sut)
	}
}

// TestSimulationExploreCommitVsWaiter explores commit / notification-
// waiter interleavings: the waiter parks on WaitVersion under a
// VIRTUAL-time bound, so a lost wake under any explored schedule
// surfaces as a deterministic, replayable violation (cross-process.md
// §Notification region, no-lost-wake; the bound rationale is at the
// WaitVersion call).
func TestSimulationExploreCommitVsWaiter(t *testing.T) {
	// A smaller budget than the sibling legs: the waiter's futex
	// park/wake and the arming handshake make each schedule's decision
	// prefix deep, and exhaustive prefix replay grows with it — 40
	// schedules keeps the leg in interactive time (stated cap, as
	// always; crank DST_SEEDS/budget for long local runs).
	waiterOpts := exploreOpts
	waiterOpts.MaxSchedules = 40
	for _, seed := range sweepSeeds(t, 63) {
		sut := func() bool {
			violation := false
			waiterDone := make(chan struct{})
			simulation.Host("h", simulation.HostConfig{}, func() {
				simulation.Process("setup", func() {
					ctx := context.Background()
					db := openDB(t, "/db")
					defer db.Close()
					if err := db.Update(ctx, func(tx *gmdb.Tx) error {
						_, e := tx.CreateKeyspace("k")
						return e
					}); err != nil {
						t.Fatalf("setup: %v", err)
					}
				})
				go simulation.Process("waiter", func() {
					defer close(waiterDone)
					db := openDB(t, "/db")
					defer db.Close()
					cur, err := db.Version()
					if err != nil {
						t.Errorf("Version: %v", err)
						return
					}
					if err := os.WriteFile("/waiter-armed", nil, 0o600); err != nil {
						t.Errorf("signal: %v", err)
						return
					}
					// A VIRTUAL-time bound turns a lost wake into a fast,
					// replayable violation: an unbounded park would leave
					// the schedule churning heartbeat timers against the
					// wedge detector instead of failing crisply (the bound
					// costs nothing when the wake arrives — virtual time
					// jumps). 10s of virtual time is far past every
					// coordination interval in smallOpts.
					wctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					defer cancel()
					// from=cur: WaitVersion returns once the version
					// EXCEEDS its argument — the writer's single commit.
					if _, err := db.WaitVersion(wctx, cur); err != nil {
						violation = true
					}
				})
				simulation.Process("writer", func() {
					ctx := context.Background()
					mustAwait(t, "/waiter-armed")
					db := openDB(t, "/db")
					defer db.Close()
					if err := db.Update(ctx, func(tx *gmdb.Tx) error {
						ks, e := tx.OpenKeyspace("k")
						if e != nil {
							return e
						}
						return ks.Put([]byte("a"), []byte("v"))
					}); err != nil {
						t.Errorf("writer: %v", err)
					}
				})
				// Join the waiter so no schedule ends with a stray parked
				// goroutine (a bubble-teardown panic, not a result).
				<-waiterDone
			})
			return violation
		}
		res := simulation.ExploreWith(seed, waiterOpts, sut)
		exploreReport(t, "commit-vs-waiter", seed, res, sut)
	}
}

// TestSimulationExploreCommitVsMaintenance explores commit /
// maintenance-daemon interleavings (reclamation, scrubbing) with the
// daemon ENABLED at a short interval — the one suite where it runs —
// and pins that every explored schedule leaves the database Check-clean
// (api-surface.md §Check; free-space.md reclamation invariants).
func TestSimulationExploreCommitVsMaintenance(t *testing.T) {
	maintOpts := smallOpts
	maintOpts.Maintenance = gmdb.MaintenanceOptions{Interval: 10 * time.Millisecond}
	for _, seed := range sweepSeeds(t, 64) {
		sut := func() bool {
			violation := false
			simulation.Host("h", simulation.HostConfig{}, func() {
				simulation.Process("app", func() {
					ctx := context.Background()
					db, err := gmdb.Open(ctx, "/db", maintOpts)
					if err != nil {
						t.Errorf("open: %v", err)
						return
					}
					defer db.Close()
					for i := range 3 {
						if err := db.Update(ctx, func(tx *gmdb.Tx) error {
							ks, e := tx.CreateKeyspaceIfNotExists("k")
							if e != nil {
								return e
							}
							return ks.Put([]byte{byte(i)}, []byte("v"))
						}); err != nil {
							t.Errorf("update %d: %v", i, err)
							return
						}
						time.Sleep(8 * time.Millisecond) // let the daemon interleave
					}
					// Check's page-accounting comparisons are advisory under
					// concurrent writers; this oracle is strict-clean only
					// because the daemon commits nothing in this tiny
					// workload (no leaks to reclaim, compaction thresholded).
					// Growing the workload needs a residue-tolerant oracle.
					for issue := range db.Check() {
						_ = issue
						violation = true
					}
				})
			})
			return violation
		}
		res := simulation.ExploreWith(seed, exploreOpts, sut)
		exploreReport(t, "commit-vs-maintenance", seed, res, sut)
	}
}

// TestSimulationPCTCommitVsReaders runs the reader surface under the
// PCT strategy (depth-3 priority inversions) across the seed sweep —
// the probabilistic complement to DPOR's systematic enumeration, per
// §Exploration tier.
func TestSimulationPCTCommitVsReaders(t *testing.T) {
	// Sweep-level reachability pin: across the seeds, PCT's priority
	// inversions must land the read on BOTH sides of the commit — a
	// sweep that only ever sees one value is not exercising the race
	// (the budgeted Exhaustive leg cannot reach the far side inside
	// its schedule cap; this sweep is where reachability is enforced).
	observed := map[string]bool{}
	for _, seed := range sweepSeeds(t, 65, 66, 67) {
		simulation.TestWith(t, seed, simulation.Options{Strategy: simulation.PCT, Depth: 3, Steps: 400}, func(t *testing.T) {
			simulation.Host("h", simulation.HostConfig{}, func() {
				simulation.Process("setup", func() {
					ctx := context.Background()
					db := openDB(t, "/db")
					defer db.Close()
					if err := db.Update(ctx, func(tx *gmdb.Tx) error {
						ks, e := tx.CreateKeyspace("k")
						if e != nil {
							return e
						}
						return ks.Put([]byte("a"), []byte("old"))
					}); err != nil {
						t.Fatalf("setup: %v", err)
					}
				})
				writerDone := make(chan struct{})
				go simulation.Process("writer", func() {
					defer close(writerDone) // after Close (LIFO)
					ctx := context.Background()
					db := openDB(t, "/db")
					defer db.Close()
					if err := db.Update(ctx, func(tx *gmdb.Tx) error {
						ks, e := tx.OpenKeyspace("k")
						if e != nil {
							return e
						}
						return ks.Put([]byte("a"), []byte("new"))
					}); err != nil {
						t.Errorf("writer: %v", err)
					}
				})
				simulation.Process("reader", func() {
					ctx := context.Background()
					db := openDB(t, "/db")
					defer db.Close()
					if err := db.View(ctx, func(rtx *gmdb.ReadTx) error {
						ks, e := rtx.OpenKeyspaceReadOnly("k")
						if e != nil {
							return e
						}
						v, e := ks.Get([]byte("a"))
						if e != nil || (string(v) != "old" && string(v) != "new") {
							t.Errorf("seed %d: reader saw %q, %v; want old or new", seed, v, e)
						} else {
							observed[string(v)] = true
						}
						return nil
					}); err != nil {
						t.Errorf("seed %d: View: %v", seed, err)
					}
				})
				<-writerDone
			})
		})
	}
	if !observed["old"] || !observed["new"] {
		t.Fatalf("PCT sweep observed only %v — the read never landed on both sides of the commit; extend the seed set", observed)
	}
}

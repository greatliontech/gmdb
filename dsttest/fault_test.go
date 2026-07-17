//go:build dst

package dsttest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"testing/simulation"
	"time"

	"github.com/greatliontech/gmdb"
)

// Fault suite (docs/specs/dst-testing.md §Suites): durability.md's commit
// outcome classification and api-surface.md's publish contracts under the
// fork's REAL disk and clock faults — what the untagged FileOps-seam tier
// cannot reach: kernel-faithful dropped-writeback across an EIO'd barrier,
// a genuinely full disk, platter rot surfacing at reboot, and wall-clock
// faults against the BOOTTIME-based liveness protocol.

// TestSimulationFsyncGateRecovery sweeps a disk fault across the commit
// pipeline (durability.md §Commit outcome classification + §Checkpoint
// failure semantics), in the two shapes a real Linux disk failure takes:
//
//   - Writeback failure (simulation.FailWriteback — ext4's default data
//     handling, the fsyncgate reality): buffered reads and writes are
//     served by the page cache, only the sync barrier fails, and the EIO'd
//     sync DROPS the file's dirty pages so a healed retry passes without
//     them. Because the classification's verification reads are page-cache
//     preads, every post-usage-check commit failure under this shape MUST
//     carry one of the three outcome classes — the certainty statement is
//     reachable end-to-end, and the recovery that works is gmdb's
//     documented re-Open + Checkpoint, never a blind fsync retry.
//   - Whole-stack failure (simulation.FailDisk — a filesystem shutdown /
//     dead controller fails every call, cached or not): the verification
//     read itself EIOs, so the classification correctly WITHHOLDS the
//     class and reports the documented unclassified fallback ("could not
//     be verified... do not retry; re-Open and probe").
//
// Each (seed, delay) anchor faults the disk at a different virtual instant
// of a SlowDisk-stretched commit; whatever the commit reports, the
// post-recovery database must survive a real power loss with exactly the
// state the report promised.
func TestSimulationFsyncGateRecovery(t *testing.T) {
	classed := 0 // anchors whose failure carried an outcome class
	for _, anchor := range []struct {
		seed  uint64
		delay time.Duration
		// shutdown selects the whole-stack FailDisk shape (the anchor
		// pinning the unclassified fallback); default is FailWriteback.
		shutdown bool
	}{
		{41, 0, false}, {41, 6 * time.Millisecond, false}, {41, 14 * time.Millisecond, false},
		{42, 10 * time.Millisecond, false}, {42, 22 * time.Millisecond, false}, {42, 34 * time.Millisecond, false},
		// Probe-verified coordinate where the whole-stack fault lands inside
		// Tx.Commit's publish steps: the failure is post-usage-check yet the
		// EIO'd verification read forces the unclassified fallback.
		{42, 22 * time.Millisecond, true},
	} {
		simulation.Test(t, anchor.seed, func(t *testing.T) {
			tag := fmt.Sprintf("seed %d delay %v shutdown %v", anchor.seed, anchor.delay, anchor.shutdown)
			var commitErr error
			writerDone := make(chan struct{})
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
						return ks.Put([]byte("k1"), []byte("v1"))
					}); err != nil {
						t.Fatalf("%s: setup: %v", tag, err)
					}
				})
				go simulation.Process("writer", func() {
					ctx := context.Background()
					db := openDB(t, "/db")
					if err := os.WriteFile("/writer-open", nil, 0o600); err != nil {
						t.Errorf("%s: signal: %v", tag, err)
						return
					}
					mustAwait(t, "/slow-armed")
					commitErr = db.Update(ctx, func(tx *gmdb.Tx) error {
						ks, e := tx.OpenKeyspace("k")
						if e != nil {
							return e
						}
						return ks.Put([]byte("k2"), bytes.Repeat([]byte("x"), 32768))
					})
					// No Close: the handle may be poisoned and the disk still
					// failing; process exit models the crashed-ish teardown.
					// The done signal is DRIVER sequencing, so it must not
					// depend on the still-failing disk — a channel, per the
					// harness convention.
					close(writerDone)
					time.Sleep(time.Hour)
				})
				simulation.Process("driver", func() {
					mustAwait(t, "/writer-open")
					simulation.SlowDisk("h", 2*time.Millisecond)
					if err := os.WriteFile("/slow-armed", nil, 0o600); err != nil {
						t.Fatalf("%s: signal: %v", tag, err)
					}
					time.Sleep(anchor.delay)
					if anchor.shutdown {
						simulation.FailDisk("h")
					} else {
						simulation.FailWriteback("h")
					}
					<-writerDone
					if anchor.shutdown {
						simulation.HealDisk("h")
					} else {
						simulation.HealWriteback("h")
					}
					simulation.SlowDisk("h", 0)
				})
				simulation.Crash("writer")
				simulation.Process("recovery", func() {
					ctx := context.Background()
					db := openDB(t, "/db")
					defer db.Close()
					// The documented re-assert path after a durability-
					// uncertain outcome: re-Open (done) + Checkpoint. A blind
					// fsync retry on the old handle would pass while the
					// dropped pages stay lost — the trap the fork models.
					if err := db.Checkpoint(ctx); err != nil {
						t.Fatalf("%s: Checkpoint: %v", tag, err)
					}
					if err := db.Update(ctx, func(tx *gmdb.Tx) error {
						ks, e := tx.OpenKeyspace("k")
						if e != nil {
							return e
						}
						return ks.Put([]byte("k3"), []byte("v3"))
					}); err != nil {
						t.Fatalf("%s: post-heal Update: %v", tag, err)
					}
				})
			})
			if anchor.shutdown {
				// The whole-stack shape: the verification read EIO'd, so the
				// class is correctly withheld and the documented fallback
				// fires with its "do not retry; re-Open and probe" guidance.
				switch {
				case commitErr == nil:
					t.Errorf("%s: shutdown anchor's commit succeeded; want the unclassified fallback", tag)
				case commitClassified(commitErr):
					t.Errorf("%s: shutdown anchor carried a class (%v); an EIO'd verification read must withhold it", tag, commitErr)
				case !strings.Contains(commitErr.Error(), "could not be verified"):
					t.Errorf("%s: shutdown anchor error %q lacks the documented unclassified-fallback shape", tag, commitErr)
				}
			} else if commitErr != nil && !commitClassified(commitErr) {
				// Under the writeback shape the verification reads are
				// cache-served and cannot fail, so every post-usage-check
				// commit failure must carry a class — an unclassified error
				// here is a classification-contract violation.
				t.Errorf("%s: writeback-fault commit failure carries no outcome class: %v", tag, commitErr)
			}
			simulation.CrashHost("h")
			simulation.Host("h", simulation.HostConfig{}, func() {
				simulation.Process("verifier", func() {
					ctx := context.Background()
					db := openDB(t, "/db")
					defer db.Close()
					// A failed commit in the pre-crash history legitimately
					// leaves DETECTABLE bitmap residue (integrity.md; the
					// recovery gate's bitmap redirty converges cache and
					// platter, so the residue is exactly the offline-
					// repairable class): BitmapLeak — pages the dead writer
					// allocated that no tree references — and, when recovery
					// took the byte-identical self-durable arm (whose meta
					// count cannot be rewritten), FreeCountMismatch against
					// that count. Anything else is a real failure; the
					// residue must repair to fully clean.
					residue := 0
					for issue := range db.Check() {
						if issue.Code == "BitmapLeak" || issue.Code == "FreeCountMismatch" {
							residue++
							continue
						}
						t.Errorf("%s: Check: %+v", tag, issue)
					}
					if residue > 0 {
						for issue := range db.CheckWithOptions(&gmdb.CheckOptions{Repair: true}) {
							if issue.Code != "BitmapLeak" && issue.Code != "FreeCountMismatch" {
								t.Errorf("%s: Repair: %+v", tag, issue)
							}
						}
						for issue := range db.Check() {
							t.Errorf("%s: post-repair Check: %+v", tag, issue)
						}
					}
					if err := db.View(ctx, func(rtx *gmdb.ReadTx) error {
						ks, e := rtx.OpenKeyspaceReadOnly("k")
						if e != nil {
							return e
						}
						if v, e := ks.Get([]byte("k1")); e != nil || string(v) != "v1" {
							t.Errorf("%s: k1 = %q, %v; want v1", tag, v, e)
						}
						if v, e := ks.Get([]byte("k3")); e != nil || string(v) != "v3" {
							t.Errorf("%s: k3 = %q, %v; want v3 (post-heal durable commit)", tag, v, e)
						}
						v2, e2 := ks.Get([]byte("k2"))
						switch {
						case commitErr == nil:
							// SyncDurable success: k2 must have survived.
							if e2 != nil || !bytes.Equal(v2, bytes.Repeat([]byte("x"), 32768)) {
								t.Errorf("%s: successful commit's k2 lost or wrong (%v)", tag, e2)
							}
						case errors.Is(commitErr, gmdb.ErrCommitNotVisible):
							if !errors.Is(e2, gmdb.ErrNotFound) {
								t.Errorf("%s: NotVisible commit's k2 = %v, want ErrNotFound", tag, e2)
							}
						default:
							// Visible / DurabilityUnknown / unclassified: k2
							// may or may not have survived the power loss,
							// but if present it is complete and exact.
							if e2 == nil && !bytes.Equal(v2, bytes.Repeat([]byte("x"), 32768)) {
								t.Errorf("%s: surviving k2 corrupt", tag)
							} else if e2 != nil && !errors.Is(e2, gmdb.ErrNotFound) {
								t.Errorf("%s: k2 read = %v, want value or ErrNotFound", tag, e2)
							}
						}
						return nil
					}); err != nil {
						t.Fatalf("%s: View: %v", tag, err)
					}
					if commitErr != nil && commitClassified(commitErr) {
						classed++
					}
				})
			})
		})
	}
	if classed == 0 {
		t.Fatalf("no anchor's failure carried an outcome class — every writeback anchor landed outside the commit window and the sweep never exercised the classification contract; retune the delays")
	}
}

// commitClassified reports whether err carries one of durability.md's three
// commit outcome classes.
func commitClassified(err error) bool {
	return errors.Is(err, gmdb.ErrCommitNotVisible) ||
		errors.Is(err, gmdb.ErrCommitVisible) ||
		errors.Is(err, gmdb.ErrCommitDurabilityUnknown)
}

// TestSimulationENOSPCCreationPaths pins the publish and open contracts on a
// genuinely full disk (api-surface.md §Check, CopyTo, Compact): CopyTo fails
// cleanly — classified error, no partial destination, no leftover temp — and
// a create-Open fails without wreckage; freeing space restores both. The
// mid-commit data-write ENOSPC is deliberately NOT here: gmdb grows its file
// by truncation, and the fork's ENOSPC model charges truncate growth without
// ENOSPC-checking it (its recorded logical-bytes boundary), so that
// classification stays pinned by the untagged FileOps-seam tier — stated in
// the spec, not silently skipped.
func TestSimulationENOSPCCreationPaths(t *testing.T) {
	simulation.Test(t, 43, func(t *testing.T) {
		simulation.Host("h", simulation.HostConfig{}, func() {
			simulation.Process("driver", func() {
				ctx := context.Background()
				db := openDB(t, "/db")
				defer db.Close()
				if err := db.Update(ctx, func(tx *gmdb.Tx) error {
					ks, e := tx.CreateKeyspace("k")
					if e != nil {
						return e
					}
					return ks.Put([]byte("k1"), []byte("v1"))
				}); err != nil {
					t.Fatalf("setup: %v", err)
				}
				var used int64
				for _, p := range []string{"/db", "/db.lock"} {
					fi, err := os.Stat(p)
					if err != nil {
						t.Fatalf("stat %s: %v", p, err)
					}
					used += fi.Size()
				}
				simulation.LimitDisk("h", used) // full: any growth or create fails
				err := db.CopyTo("/backup.db", false)
				if err == nil {
					t.Fatal("CopyTo on a full disk succeeded")
				}
				if _, serr := os.Lstat("/backup.db"); !errors.Is(serr, os.ErrNotExist) {
					t.Fatalf("CopyTo failure left a destination: %v", serr)
				}
				entries, derr := os.ReadDir("/")
				if derr != nil {
					t.Fatalf("ReadDir: %v", derr)
				}
				for _, e := range entries {
					if len(e.Name()) > 9 && e.Name()[:9] == "backup.db" {
						t.Fatalf("CopyTo failure left wreckage %q", e.Name())
					}
				}
				if _, oerr := gmdb.Open(ctx, "/other.db", smallOpts); oerr == nil {
					t.Fatal("create-Open on a full disk succeeded")
				}
				simulation.UnlimitDisk("h")
				if err := db.CopyTo("/backup.db", false); err != nil {
					t.Fatalf("CopyTo after freeing space: %v", err)
				}
				for issue := range db.Check() {
					t.Errorf("Check: %+v", issue)
				}
			})
		})
	})
}

// TestSimulationSlowDiskNoFalseStale pins latency vs liveness (cross-
// process.md heartbeats): a commit stretched past StaleTimeout by a slow
// disk must not get its writer taken over — the heartbeat goroutine beats in
// shared memory, untouched by the disk, so a peer classifies the slow writer
// LIVE throughout, and both writers' rows land.
func TestSimulationSlowDiskNoFalseStale(t *testing.T) {
	opts := smallOpts
	opts.HeartbeatInterval = 50 * time.Millisecond
	opts.StaleTimeout = 300 * time.Millisecond
	simulation.Test(t, 44, func(t *testing.T) {
		simulation.Host("h", simulation.HostConfig{}, func() {
			var slowStart, slowEnd time.Time
			simulation.Process("setup", func() {
				ctx := context.Background()
				db, err := gmdb.Open(ctx, "/db", opts)
				if err != nil {
					t.Fatalf("Open: %v", err)
				}
				defer db.Close()
				if err := db.Update(ctx, func(tx *gmdb.Tx) error {
					_, e := tx.CreateKeyspace("k")
					return e
				}); err != nil {
					t.Fatalf("setup: %v", err)
				}
			})
			done := make(chan struct{})
			go simulation.Process("slow-writer", func() {
				ctx := context.Background()
				db, err := gmdb.Open(ctx, "/db", opts)
				if err != nil {
					t.Errorf("slow Open: %v", err)
					close(done)
					return
				}
				defer db.Close()
				simulation.SlowDisk("h", 40*time.Millisecond)
				slowStart = time.Now()
				if err := db.Update(ctx, func(tx *gmdb.Tx) error {
					ks, e := tx.OpenKeyspace("k")
					if e != nil {
						return e
					}
					return ks.Put([]byte("slow"), bytes.Repeat([]byte("s"), 8192))
				}); err != nil {
					t.Errorf("slow Update: %v", err)
				}
				slowEnd = time.Now()
				simulation.SlowDisk("h", 0)
				close(done)
			})
			<-done
			if el := slowEnd.Sub(slowStart); el <= opts.StaleTimeout {
				t.Fatalf("slow commit took %v, want > StaleTimeout %v (the construction must stress the window)", el, opts.StaleTimeout)
			}
			simulation.Process("peer", func() {
				ctx := context.Background()
				db, err := gmdb.Open(ctx, "/db", opts)
				if err != nil {
					t.Fatalf("peer Open: %v", err)
				}
				defer db.Close()
				if err := db.Update(ctx, func(tx *gmdb.Tx) error {
					ks, e := tx.OpenKeyspace("k")
					if e != nil {
						return e
					}
					return ks.Put([]byte("peer"), []byte("p"))
				}); err != nil {
					t.Fatalf("peer Update: %v", err)
				}
				for issue := range db.Check() {
					t.Errorf("Check: %+v", issue)
				}
				if err := db.View(ctx, func(rtx *gmdb.ReadTx) error {
					ks, e := rtx.OpenKeyspaceReadOnly("k")
					if e != nil {
						return e
					}
					if _, e := ks.Get([]byte("slow")); e != nil {
						t.Errorf("slow writer's row lost: %v (false takeover?)", e)
					}
					if _, e := ks.Get([]byte("peer")); e != nil {
						t.Errorf("peer's row lost: %v", e)
					}
					return nil
				}); err != nil {
					t.Fatalf("View: %v", err)
				}
			})
		})
	})
}

// TestSimulationBitRotDetectedAtReboot pins checksums.md's contract against
// the fork's CorruptFile fault: a bit flipped on the platter surfaces at the
// next real read of the platter — the post-reboot open. The database must
// never serve wrong bytes silently: every seed either detects the rot (open
// error, Check issue, or a checksum-failing read) or proves the flip landed
// outside live data by reading everything back byte-exact. The sweep asserts
// at least one seed lands in live data, so the leg cannot pass vacuously.
func TestSimulationBitRotDetectedAtReboot(t *testing.T) {
	detected := 0
	payload := bytes.Repeat([]byte("gmdb-bit-rot-payload-"), 512)
	for _, seed := range []uint64{45, 46, 47, 48, 49, 50} {
		simulation.Test(t, seed, func(t *testing.T) {
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
						for i := range 8 {
							if e := ks.Put(fmt.Appendf(nil, "row-%d", i), payload); e != nil {
								return e
							}
						}
						return nil
					}); err != nil {
						t.Fatalf("seed %d: setup: %v", seed, err)
					}
				})
				simulation.Process("rot", func() {
					simulation.CorruptFile("h", "/db")
				})
			})
			simulation.CrashHost("h")
			simulation.Host("h", simulation.HostConfig{}, func() {
				simulation.Process("verifier", func() {
					ctx := context.Background()
					db, err := gmdb.Open(ctx, "/db", smallOpts)
					if err != nil {
						detected++ // rot landed in a page Open verifies
						return
					}
					defer db.Close()
					issues := 0
					for range db.Check() {
						issues++
					}
					readErr := db.View(ctx, func(rtx *gmdb.ReadTx) error {
						ks, e := rtx.OpenKeyspaceReadOnly("k")
						if e != nil {
							return e
						}
						for i := range 8 {
							v, e := ks.Get(fmt.Appendf(nil, "row-%d", i))
							if e != nil {
								return e
							}
							if !bytes.Equal(v, payload) {
								t.Fatalf("seed %d: row-%d served WRONG BYTES silently — the failure checksums exist to prevent", seed, i)
							}
						}
						return nil
					})
					if issues > 0 || readErr != nil {
						detected++
					}
				})
			})
		})
	}
	if detected == 0 {
		t.Fatalf("no seed's flip was detected or landed in live data — the sweep is vacuous; widen the payload or seeds")
	}
}

// TestSimulationWallClockFaultImmunity pins the liveness protocol's clock
// choice (cross-process.md: heartbeats and windows on CLOCK_BOOTTIME, which
// wall steps and drift never move): hour-scale wall steps in both
// directions plus drift, injected mid-coordination, perturb nothing — no
// false stale, no frozen liveness, takeover after a real crash still works.
func TestSimulationWallClockFaultImmunity(t *testing.T) {
	opts := smallOpts
	opts.HeartbeatInterval = 50 * time.Millisecond
	opts.StaleTimeout = 300 * time.Millisecond
	simulation.Test(t, 51, func(t *testing.T) {
		simulation.Host("h", simulation.HostConfig{}, func() {
			simulation.Process("setup", func() {
				ctx := context.Background()
				db, err := gmdb.Open(ctx, "/db", opts)
				if err != nil {
					t.Fatalf("Open: %v", err)
				}
				defer db.Close()
				if err := db.Update(ctx, func(tx *gmdb.Tx) error {
					_, e := tx.CreateKeyspace("k")
					return e
				}); err != nil {
					t.Fatalf("setup: %v", err)
				}
			})
			wallBefore := time.Now()
			simulation.StepClock("h", -2*time.Hour)
			simulation.DriftClock("h", 200_000_000) // rate 1.2
			if d := time.Since(wallBefore); d > -time.Hour {
				t.Fatalf("wall step not applied (delta %v)", d)
			}
			go simulation.Process("victim", func() {
				ctx := context.Background()
				db, err := gmdb.Open(ctx, "/db", opts)
				if err != nil {
					t.Errorf("victim Open: %v", err)
					return
				}
				_ = db.Update(ctx, func(tx *gmdb.Tx) error {
					ks, e := tx.OpenKeyspace("k")
					if e != nil {
						return e
					}
					if e := ks.Put([]byte("victim"), []byte("v")); e != nil {
						return e
					}
					if e := os.WriteFile("/victim-in-tx", nil, 0o600); e != nil {
						return e
					}
					time.Sleep(time.Hour)
					return nil
				})
				t.Error("victim survived its crash")
			})
			simulation.Process("survivor", func() {
				ctx := context.Background()
				mustAwait(t, "/victim-in-tx")
				simulation.StepClock("h", 5*time.Hour) // forward mid-coordination
				simulation.Crash("victim")
				db, err := gmdb.Open(ctx, "/db", opts)
				if err != nil {
					t.Fatalf("survivor Open: %v", err)
				}
				defer db.Close()
				if err := db.Update(ctx, func(tx *gmdb.Tx) error {
					ks, e := tx.OpenKeyspace("k")
					if e != nil {
						return e
					}
					return ks.Put([]byte("survivor"), []byte("s"))
				}); err != nil {
					t.Fatalf("takeover Update under stepped clock: %v", err)
				}
				for issue := range db.Check() {
					t.Errorf("Check: %+v", issue)
				}
			})
		})
	})
}

// TestSimulationFsyncGateSameProcessReopen pins the LIVE-classified arm of
// the bitmap-redirty duty (durability.md §Anchoring): after a writeback-
// shape publication failure, the same process follows the documented
// recovery — Close + re-Open + Checkpoint — whose Open classifies live
// (the writer record survives) and must redirty the bitmap region it
// attached, or the dropped bitmap pwrites stay clean-stale and the
// checkpoint anchors over bytes the platter never received.
func TestSimulationFsyncGateSameProcessReopen(t *testing.T) {
	simulation.Test(t, 53, func(t *testing.T) {
		var commitErr error
		simulation.Host("h", simulation.HostConfig{}, func() {
			simulation.Process("app", func() {
				ctx := context.Background()
				db := openDB(t, "/db")
				if err := db.Update(ctx, func(tx *gmdb.Tx) error {
					ks, e := tx.CreateKeyspace("k")
					if e != nil {
						return e
					}
					return ks.Put([]byte("k1"), []byte("v1"))
				}); err != nil {
					t.Fatalf("setup: %v", err)
				}
				simulation.FailWriteback("h")
				commitErr = db.Update(ctx, func(tx *gmdb.Tx) error {
					ks, e := tx.OpenKeyspace("k")
					if e != nil {
						return e
					}
					return ks.Put([]byte("k2"), bytes.Repeat([]byte("y"), 32768))
				})
				simulation.HealWriteback("h")
				if commitErr == nil || !commitClassified(commitErr) {
					t.Fatalf("faulted commit = %v; want a classified failure", commitErr)
				}
				db.Close()
				// The documented recovery, same process: re-Open (live-
				// classified) + Checkpoint.
				db2 := openDB(t, "/db")
				defer db2.Close()
				if err := db2.Checkpoint(ctx); err != nil {
					t.Fatalf("Checkpoint: %v", err)
				}
				if err := db2.Update(ctx, func(tx *gmdb.Tx) error {
					ks, e := tx.OpenKeyspace("k")
					if e != nil {
						return e
					}
					return ks.Put([]byte("k3"), []byte("v3"))
				}); err != nil {
					t.Fatalf("post-recovery Update: %v", err)
				}
			})
		})
		simulation.CrashHost("h")
		simulation.Host("h", simulation.HostConfig{}, func() {
			simulation.Process("verifier", func() {
				ctx := context.Background()
				db := openDB(t, "/db")
				defer db.Close()
				residue := 0
				for issue := range db.Check() {
					if issue.Code == "BitmapLeak" || issue.Code == "FreeCountMismatch" {
						residue++
						continue
					}
					t.Errorf("Check: %+v", issue)
				}
				if residue > 0 {
					for issue := range db.CheckWithOptions(&gmdb.CheckOptions{Repair: true}) {
						if issue.Code != "BitmapLeak" && issue.Code != "FreeCountMismatch" {
							t.Errorf("Repair: %+v", issue)
						}
					}
					for issue := range db.Check() {
						t.Errorf("post-repair Check: %+v", issue)
					}
				}
				if err := db.View(ctx, func(rtx *gmdb.ReadTx) error {
					ks, e := rtx.OpenKeyspaceReadOnly("k")
					if e != nil {
						return e
					}
					if v, e := ks.Get([]byte("k1")); e != nil || string(v) != "v1" {
						t.Errorf("k1 = %q, %v; want v1", v, e)
					}
					if v, e := ks.Get([]byte("k3")); e != nil || string(v) != "v3" {
						t.Errorf("k3 = %q, %v; want v3", v, e)
					}
					if v2, e2 := ks.Get([]byte("k2")); e2 == nil {
						if !bytes.Equal(v2, bytes.Repeat([]byte("y"), 32768)) {
							t.Errorf("surviving k2 corrupt")
						}
					} else if !errors.Is(e2, gmdb.ErrNotFound) {
						t.Errorf("k2 = %v, want value or ErrNotFound", e2)
					}
					return nil
				}); err != nil {
					t.Fatalf("View: %v", err)
				}
			})
		})
	})
}

// TestSimulationFsyncGatePeerTakeoverResync pins the forced-resync leg of
// the bitmap-redirty duty (durability.md §Anchoring): a writer poisons on a
// writeback-shape publication failure (bumping the takeover sequence under
// its grant) and SURVIVES; a peer's next grant acquisition sees the lag,
// forces the rebuild, and must redirty the bitmap region it attached — its
// own barrier then genuinely covers the dropped bitmap bytes.
func TestSimulationFsyncGatePeerTakeoverResync(t *testing.T) {
	simulation.Test(t, 54, func(t *testing.T) {
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
					return ks.Put([]byte("k1"), []byte("v1"))
				}); err != nil {
					t.Fatalf("setup: %v", err)
				}
			})
			go simulation.Process("peer", func() {
				ctx := context.Background()
				db := openDB(t, "/db")
				if err := os.WriteFile("/peer-open", nil, 0o600); err != nil {
					t.Errorf("signal: %v", err)
					return
				}
				mustAwait(t, "/writer-poisoned")
				// Grant acquisition sees the poison-site bump → forced
				// resync → redirty; the commit's barrier covers it.
				if err := db.Update(ctx, func(tx *gmdb.Tx) error {
					ks, e := tx.OpenKeyspace("k")
					if e != nil {
						return e
					}
					return ks.Put([]byte("k3"), []byte("v3"))
				}); err != nil {
					t.Errorf("peer Update: %v", err)
				}
				if err := os.WriteFile("/peer-done", nil, 0o600); err != nil {
					t.Errorf("signal: %v", err)
				}
				time.Sleep(time.Hour)
			})
			go simulation.Process("writer", func() {
				ctx := context.Background()
				mustAwait(t, "/peer-open")
				db := openDB(t, "/db")
				simulation.FailWriteback("h")
				commitErr := db.Update(ctx, func(tx *gmdb.Tx) error {
					ks, e := tx.OpenKeyspace("k")
					if e != nil {
						return e
					}
					return ks.Put([]byte("k2"), bytes.Repeat([]byte("z"), 32768))
				})
				simulation.HealWriteback("h")
				if commitErr == nil || !commitClassified(commitErr) {
					t.Errorf("faulted commit = %v; want a classified failure", commitErr)
				}
				if err := os.WriteFile("/writer-poisoned", nil, 0o600); err != nil {
					t.Errorf("signal: %v", err)
				}
				// Stay alive: the writer record must classify LIVE so the
				// peer takes the forced-resync path, never the gated arm.
				time.Sleep(time.Hour)
			})
			simulation.Process("await", func() { mustAwait(t, "/peer-done") })
		})
		simulation.CrashHost("h")
		simulation.Host("h", simulation.HostConfig{}, func() {
			simulation.Process("verifier", func() {
				ctx := context.Background()
				db := openDB(t, "/db")
				defer db.Close()
				residue := 0
				for issue := range db.Check() {
					if issue.Code == "BitmapLeak" || issue.Code == "FreeCountMismatch" {
						residue++
						continue
					}
					t.Errorf("Check: %+v", issue)
				}
				if residue > 0 {
					for issue := range db.CheckWithOptions(&gmdb.CheckOptions{Repair: true}) {
						if issue.Code != "BitmapLeak" && issue.Code != "FreeCountMismatch" {
							t.Errorf("Repair: %+v", issue)
						}
					}
					for issue := range db.Check() {
						t.Errorf("post-repair Check: %+v", issue)
					}
				}
				if err := db.View(ctx, func(rtx *gmdb.ReadTx) error {
					ks, e := rtx.OpenKeyspaceReadOnly("k")
					if e != nil {
						return e
					}
					if v, e := ks.Get([]byte("k1")); e != nil || string(v) != "v1" {
						t.Errorf("k1 = %q, %v; want v1", v, e)
					}
					if v, e := ks.Get([]byte("k3")); e != nil || string(v) != "v3" {
						t.Errorf("k3 = %q, %v; want v3 (peer's post-takeover durable commit)", v, e)
					}
					return nil
				}); err != nil {
					t.Fatalf("View: %v", err)
				}
			})
		})
	})
}

// TestSimulationFsyncGateCheckpointPoisonResync is the Checkpoint-side
// twin of the peer-takeover leg: a SyncLazy writer's CHECKPOINT fails
// under the writeback shape (dropping the lazy tail's dirty pages),
// poisons, and bumps the takeover sequence — Checkpoint's poison site
// carries the same level-triggered duty as the commit pipeline's — so
// the surviving peer's forced resync redirties what the failed barrier
// dropped before anchoring over it.
func TestSimulationFsyncGateCheckpointPoisonResync(t *testing.T) {
	simulation.Test(t, 55, func(t *testing.T) {
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
					return ks.Put([]byte("k1"), []byte("v1"))
				}); err != nil {
					t.Fatalf("setup: %v", err)
				}
			})
			go simulation.Process("peer", func() {
				ctx := context.Background()
				db := openDB(t, "/db")
				if err := os.WriteFile("/peer-open", nil, 0o600); err != nil {
					t.Errorf("signal: %v", err)
					return
				}
				mustAwait(t, "/writer-poisoned")
				if err := db.Update(ctx, func(tx *gmdb.Tx) error {
					ks, e := tx.OpenKeyspace("k")
					if e != nil {
						return e
					}
					return ks.Put([]byte("k3"), []byte("v3"))
				}); err != nil {
					t.Errorf("peer Update: %v", err)
				}
				if err := os.WriteFile("/peer-done", nil, 0o600); err != nil {
					t.Errorf("signal: %v", err)
				}
				time.Sleep(time.Hour)
			})
			go simulation.Process("writer", func() {
				ctx := context.Background()
				mustAwait(t, "/peer-open")
				lazyOpts := smallOpts
				lazyOpts.SyncMode = gmdb.SyncLazy
				db, err := gmdb.Open(ctx, "/db", lazyOpts)
				if err != nil {
					t.Errorf("lazy open: %v", err)
					return
				}
				if err := db.Update(ctx, func(tx *gmdb.Tx) error {
					ks, e := tx.OpenKeyspace("k")
					if e != nil {
						return e
					}
					return ks.Put([]byte("k2"), bytes.Repeat([]byte("w"), 32768))
				}); err != nil {
					t.Errorf("lazy Update: %v", err)
					return
				}
				simulation.FailWriteback("h")
				ckErr := db.Checkpoint(ctx)
				simulation.HealWriteback("h")
				if ckErr == nil {
					t.Error("Checkpoint under FailWriteback succeeded; want a poisoning failure")
				}
				if err := os.WriteFile("/writer-poisoned", nil, 0o600); err != nil {
					t.Errorf("signal: %v", err)
				}
				time.Sleep(time.Hour) // stay live: the peer must take the forced-resync path
			})
			simulation.Process("await", func() { mustAwait(t, "/peer-done") })
		})
		simulation.CrashHost("h")
		simulation.Host("h", simulation.HostConfig{}, func() {
			simulation.Process("verifier", func() {
				ctx := context.Background()
				db := openDB(t, "/db")
				defer db.Close()
				residue := 0
				for issue := range db.Check() {
					if issue.Code == "BitmapLeak" || issue.Code == "FreeCountMismatch" {
						residue++
						continue
					}
					t.Errorf("Check: %+v", issue)
				}
				if residue > 0 {
					for issue := range db.CheckWithOptions(&gmdb.CheckOptions{Repair: true}) {
						if issue.Code != "BitmapLeak" && issue.Code != "FreeCountMismatch" {
							t.Errorf("Repair: %+v", issue)
						}
					}
					for issue := range db.Check() {
						t.Errorf("post-repair Check: %+v", issue)
					}
				}
				if err := db.View(ctx, func(rtx *gmdb.ReadTx) error {
					ks, e := rtx.OpenKeyspaceReadOnly("k")
					if e != nil {
						return e
					}
					if v, e := ks.Get([]byte("k1")); e != nil || string(v) != "v1" {
						t.Errorf("k1 = %q, %v; want v1", v, e)
					}
					if v, e := ks.Get([]byte("k3")); e != nil || string(v) != "v3" {
						t.Errorf("k3 = %q, %v; want v3", v, e)
					}
					if v2, e2 := ks.Get([]byte("k2")); e2 == nil {
						if !bytes.Equal(v2, bytes.Repeat([]byte("w"), 32768)) {
							t.Errorf("surviving k2 corrupt")
						}
					} else if !errors.Is(e2, gmdb.ErrNotFound) {
						t.Errorf("k2 = %v, want value or ErrNotFound", e2)
					}
					return nil
				}); err != nil {
					t.Fatalf("View: %v", err)
				}
			})
		})
	})
}

// TestSimulationFsyncGateLazyReopenExtent pins the full-extent form of the
// live-classified Open's redirty: a SyncLazy tail is VISIBLE when its
// checkpoint fails (dropping the tail's data pages), and the same
// process's documented recovery — Close + re-Open + Checkpoint — must
// make that tail genuinely durable, which requires redirtying the data
// pages the failed barrier dropped, not the bitmap alone.
func TestSimulationFsyncGateLazyReopenExtent(t *testing.T) {
	simulation.Test(t, 56, func(t *testing.T) {
		lazyOpts := smallOpts
		lazyOpts.SyncMode = gmdb.SyncLazy
		payload := bytes.Repeat([]byte("q"), 32768)
		simulation.Host("h", simulation.HostConfig{}, func() {
			simulation.Process("app", func() {
				ctx := context.Background()
				db := openDB(t, "/db")
				if err := db.Update(ctx, func(tx *gmdb.Tx) error {
					ks, e := tx.CreateKeyspace("k")
					if e != nil {
						return e
					}
					return ks.Put([]byte("k1"), []byte("v1"))
				}); err != nil {
					t.Fatalf("setup: %v", err)
				}
				db.Close()
				lazy, err := gmdb.Open(ctx, "/db", lazyOpts)
				if err != nil {
					t.Fatalf("lazy open: %v", err)
				}
				if err := lazy.Update(ctx, func(tx *gmdb.Tx) error {
					ks, e := tx.OpenKeyspace("k")
					if e != nil {
						return e
					}
					return ks.Put([]byte("k2"), payload)
				}); err != nil {
					t.Fatalf("lazy Update: %v", err)
				}
				simulation.FailWriteback("h")
				if err := lazy.Checkpoint(ctx); err == nil {
					t.Fatal("Checkpoint under FailWriteback succeeded; want a poisoning failure")
				}
				simulation.HealWriteback("h")
				lazy.Close()
				// Documented recovery: re-Open (live-classified — the k2
				// tail is visible and must not roll back) + Checkpoint,
				// which must make the whole visible state durable.
				db2, err := gmdb.Open(ctx, "/db", lazyOpts)
				if err != nil {
					t.Fatalf("re-open: %v", err)
				}
				defer db2.Close()
				if err := db2.Checkpoint(ctx); err != nil {
					t.Fatalf("recovery Checkpoint: %v", err)
				}
			})
		})
		simulation.CrashHost("h")
		simulation.Host("h", simulation.HostConfig{}, func() {
			simulation.Process("verifier", func() {
				ctx := context.Background()
				db := openDB(t, "/db")
				defer db.Close()
				residue := 0
				for issue := range db.Check() {
					if issue.Code == "BitmapLeak" || issue.Code == "FreeCountMismatch" {
						residue++
						continue
					}
					t.Errorf("Check: %+v", issue)
				}
				if residue > 0 {
					for issue := range db.CheckWithOptions(&gmdb.CheckOptions{Repair: true}) {
						if issue.Code != "BitmapLeak" && issue.Code != "FreeCountMismatch" {
							t.Errorf("Repair: %+v", issue)
						}
					}
					for issue := range db.Check() {
						t.Errorf("post-repair Check: %+v", issue)
					}
				}
				if err := db.View(ctx, func(rtx *gmdb.ReadTx) error {
					ks, e := rtx.OpenKeyspaceReadOnly("k")
					if e != nil {
						return e
					}
					if v, e := ks.Get([]byte("k1")); e != nil || string(v) != "v1" {
						t.Errorf("k1 = %q, %v; want v1", v, e)
					}
					// The recovery Checkpoint SUCCEEDED: its durability
					// promise covers the visible k2 tail unconditionally.
					if v, e := ks.Get([]byte("k2")); e != nil || !bytes.Equal(v, payload) {
						t.Errorf("k2 lost or wrong after a successful recovery Checkpoint (%v)", e)
					}
					return nil
				}); err != nil {
					t.Fatalf("View: %v", err)
				}
			})
		})
	})
}

//go:build dst

package dsttest

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"testing/simulation"
	"time"

	"github.com/greatliontech/gmdb"
)

// Coordination suite (docs/specs/dst-testing.md §Suites): the
// cross-process.md walk over real multi-process topology — every gmdb
// "process" is a simulation Process on one Host, sharing the data and
// lock files through the simulated page cache exactly as co-located
// OS processes would. Cross-node choreography uses the shared host
// filesystem (the crash-sound discipline); harness-level result
// channels only collect assertions.

var smallOpts = gmdb.Options{PageSize: 4096, MinSize: 16, MaxSize: 256,
	Maintenance: gmdb.MaintenanceOptions{Disable: true}}

func openDB(t *testing.T, path string) *gmdb.DB {
	t.Helper()
	db, err := gmdb.Open(context.Background(), path, smallOpts)
	if err != nil {
		t.Fatalf("Open(%s): %v", path, err)
	}
	return db
}

// awaitFile parks on the virtual clock until path exists — the
// filesystem-only cross-process signal. Bounded: a producer that died
// before signalling fails the wait crisply instead of burning the
// test to its -timeout.
func awaitFile(t *testing.T, path string) error {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := os.Lstat(path); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("signal file %s never appeared", path)
		}
		time.Sleep(time.Millisecond)
	}
}

func mustAwait(t *testing.T, path string) {
	t.Helper()
	if err := awaitFile(t, path); err != nil {
		t.Fatal(err)
	}
}

// Writer-grant mutual exclusion and handoff under contention
// (cross-process.md §Write Lock): three writer processes hammer the
// same database; every Update must serialize through the grant, no
// write may be lost, and the final state must Check clean.
func TestSimulationWriterContention(t *testing.T) {
	const writers, rows = 3, 8
	simulation.Test(t, 1, func(t *testing.T) {
		simulation.Host("h", simulation.HostConfig{}, func() {
			done := make(chan error, writers)
			for w := range writers {
				go simulation.Process(fmt.Sprintf("writer-%d", w), func() {
					done <- func() error {
						ctx := context.Background()
						db, err := gmdb.Open(ctx, "/db", smallOpts)
						if err != nil {
							return fmt.Errorf("open: %w", err)
						}
						defer db.Close()
						for i := range rows {
							if err := db.Update(ctx, func(tx *gmdb.Tx) error {
								ks, e := tx.CreateKeyspace("k")
								if e != nil {
									if ks, e = tx.OpenKeyspace("k"); e != nil {
										return e
									}
								}
								return ks.Put(fmt.Appendf(nil, "w%d-%02d", w, i), []byte("v"))
							}); err != nil {
								return fmt.Errorf("update %d: %w", i, err)
							}
						}
						return nil
					}()
				})
			}
			for range writers {
				if err := <-done; err != nil {
					t.Fatalf("writer: %v", err)
				}
			}
			simulation.Process("verifier", func() {
				ctx := context.Background()
				db := openDB(t, "/db")
				defer db.Close()
				for issue := range db.Check() {
					t.Errorf("Check: %+v", issue)
				}
				n := 0
				if err := db.View(ctx, func(rtx *gmdb.ReadTx) error {
					ks, e := rtx.OpenKeyspaceReadOnly("k")
					if e != nil {
						return e
					}
					for range ks.All() {
						n++
					}
					return ks.Err()
				}); err != nil {
					t.Fatalf("View: %v", err)
				}
				if n != writers*rows {
					t.Fatalf("rows = %d, want %d (a write was lost or duplicated)", n, writers*rows)
				}
			})
		})
	})
}

// Grant takeover after a mid-transaction crash (cross-process.md
// §Write Lock + §Stale writer recovery): the crash releases the
// victim's flock (kernel exit semantics), the contender recovers the
// nonzero writer record unconditionally under LOCK_EX, the victim's
// UNCOMMITTED mutation is invisible (CoW), and prior committed data
// survives. The liveness CLASSIFIER is pinned separately by
// TestSimulationRecoveryGateDeadVsLiveAuthor — here both gate
// branches coincide (durable == latest).
func TestSimulationStaleWriterTakeover(t *testing.T) {
	simulation.Test(t, 2, func(t *testing.T) {
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
					return ks.Put([]byte("committed"), []byte("v"))
				}); err != nil {
					t.Fatalf("setup Update: %v", err)
				}
			})
			go simulation.Process("victim", func() {
				ctx := context.Background()
				db := openDB(t, "/db")
				_ = db.Update(ctx, func(tx *gmdb.Tx) error {
					ks, e := tx.OpenKeyspace("k")
					if e != nil {
						return e
					}
					if e := ks.Put([]byte("uncommitted"), []byte("v")); e != nil {
						return e
					}
					if e := os.WriteFile("/victim-in-tx", nil, 0o600); e != nil {
						return e
					}
					time.Sleep(time.Hour) // holds the grant until the crash
					return nil
				})
				t.Error("victim survived its crash")
			})
			simulation.Process("contender", func() {
				ctx := context.Background()
				mustAwait(t, "/victim-in-tx")
				simulation.Crash("victim")
				db := openDB(t, "/db")
				defer db.Close()
				if err := db.Update(ctx, func(tx *gmdb.Tx) error {
					ks, e := tx.OpenKeyspace("k")
					if e != nil {
						return e
					}
					return ks.Put([]byte("takeover"), []byte("v"))
				}); err != nil {
					t.Fatalf("takeover Update: %v", err)
				}
				for issue := range db.Check() {
					t.Errorf("Check: %+v", issue)
				}
				if err := db.View(ctx, func(rtx *gmdb.ReadTx) error {
					ks, e := rtx.OpenKeyspaceReadOnly("k")
					if e != nil {
						return e
					}
					if _, e := ks.Get([]byte("committed")); e != nil {
						t.Errorf("committed row lost: %v", e)
					}
					if _, e := ks.Get([]byte("takeover")); e != nil {
						t.Errorf("takeover row missing: %v", e)
					}
					if _, e := ks.Get([]byte("uncommitted")); !errors.Is(e, gmdb.ErrNotFound) {
						t.Errorf("victim's uncommitted row = %v, want ErrNotFound", e)
					}
					return nil
				}); err != nil {
					t.Fatalf("View: %v", err)
				}
			})
		})
	})
}

// Reader-slot reaping (cross-process.md §Reader Table, stale
// detection): a crashed reader's slot — dead pid, frozen heartbeat —
// must be reclaimable so later readers can acquire when the table is
// full.
func TestSimulationReaderCrashSlotReap(t *testing.T) {
	opts := smallOpts
	opts.MaxReaders = 2
	simulation.Test(t, 3, func(t *testing.T) {
		simulation.Host("h", simulation.HostConfig{}, func() {
			simulation.Process("setup", func() {
				ctx := context.Background()
				db, err := gmdb.Open(ctx, "/db", opts)
				if err != nil {
					t.Fatalf("Open: %v", err)
				}
				defer db.Close()
				if err := db.Update(ctx, func(tx *gmdb.Tx) error {
					ks, e := tx.CreateKeyspace("k")
					if e != nil {
						return e
					}
					return ks.Put([]byte("a"), []byte("v"))
				}); err != nil {
					t.Fatalf("Update: %v", err)
				}
			})
			go simulation.Process("dead-reader", func() {
				ctx := context.Background()
				db, err := gmdb.Open(ctx, "/db", opts)
				if err != nil {
					t.Errorf("Open: %v", err)
					return
				}
				rtx, err := db.BeginRead(ctx)
				if err != nil {
					t.Errorf("BeginRead: %v", err)
					return
				}
				_ = rtx
				if err := os.WriteFile("/reader-pinned", nil, 0o600); err != nil {
					t.Errorf("signal: %v", err)
					return
				}
				time.Sleep(time.Hour) // slot held until the crash
				t.Error("dead-reader survived its crash")
			})
			simulation.Process("survivor", func() {
				ctx := context.Background()
				mustAwait(t, "/reader-pinned")
				simulation.Crash("dead-reader")
				db, err := gmdb.Open(ctx, "/db", opts)
				if err != nil {
					t.Fatalf("Open: %v", err)
				}
				defer db.Close()
				// Two concurrent read transactions need BOTH slots — the
				// second is available only if the dead reader's slot is
				// classified stale (pid-liveness) and cleared with the
				// spec's release-ordering.
				r1, err := db.BeginRead(ctx)
				if err != nil {
					t.Fatalf("BeginRead 1: %v", err)
				}
				defer r1.Rollback()
				r2, err := db.BeginRead(ctx)
				if err != nil {
					t.Fatalf("BeginRead 2 (needs the reaped slot): %v", err)
				}
				defer r2.Rollback()
			})
		})
	})
}

// Snapshot pinning across a writer's commits (cross-process.md
// §Reader Table + free-space reclamation bound): a reader's snapshot
// must stay byte-stable while a peer writer churns pages hard enough
// to force reuse; the pinned slot is what holds reclamation back.
func TestSimulationSnapshotPinnedAcrossCommits(t *testing.T) {
	simulation.Test(t, 4, func(t *testing.T) {
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
					return ks.Put([]byte("pinned"), []byte("original"))
				}); err != nil {
					t.Fatalf("Update: %v", err)
				}
			})
			readerDone := make(chan error, 1)
			go simulation.Process("reader", func() {
				readerDone <- func() error {
					ctx := context.Background()
					db, err := gmdb.Open(ctx, "/db", smallOpts)
					if err != nil {
						return err
					}
					defer db.Close()
					rtx, err := db.BeginRead(ctx)
					if err != nil {
						return err
					}
					defer rtx.Rollback()
					ks, err := rtx.OpenKeyspaceReadOnly("k")
					if err != nil {
						return err
					}
					if err := os.WriteFile("/reader-snapshotted", nil, 0o600); err != nil {
						return err
					}
					if err := awaitFile(t, "/writer-churned"); err != nil {
						return err
					}
					v, err := ks.Get([]byte("pinned"))
					if err != nil {
						return fmt.Errorf("snapshot read after churn: %w", err)
					}
					if string(v) != "original" {
						return fmt.Errorf("snapshot drifted: %q", v)
					}
					return nil
				}()
			})
			simulation.Process("writer", func() {
				ctx := context.Background()
				mustAwait(t, "/reader-snapshotted")
				db := openDB(t, "/db")
				defer db.Close()
				for i := range 30 {
					if err := db.Update(ctx, func(tx *gmdb.Tx) error {
						ks, e := tx.OpenKeyspace("k")
						if e != nil {
							return e
						}
						if e := ks.Put([]byte("pinned"), fmt.Appendf(nil, "overwrite-%02d", i)); e != nil {
							return e
						}
						return ks.Put(fmt.Appendf(nil, "churn-%02d", i), make([]byte, 512))
					}); err != nil {
						t.Fatalf("churn %d: %v", i, err)
					}
				}
				if err := os.WriteFile("/writer-churned", nil, 0o600); err != nil {
					t.Fatalf("signal: %v", err)
				}
			})
			if err := <-readerDone; err != nil {
				t.Fatalf("reader: %v", err)
			}
		})
	})
}

// Notification under contention (cross-process.md §Notification
// region): a cancelled waiter's wake is spurious for its peers —
// cancellation must not consume the publish that a surviving waiter
// in ANOTHER process is parked on (no lost wake), and the survivor
// observes the published version.
func TestSimulationNotificationNoLostWake(t *testing.T) {
	simulation.Test(t, 5, func(t *testing.T) {
		simulation.Host("h", simulation.HostConfig{}, func() {
			simulation.Process("setup", func() {
				ctx := context.Background()
				db := openDB(t, "/db")
				defer db.Close()
				if err := db.Update(ctx, func(tx *gmdb.Tx) error {
					_, e := tx.CreateKeyspace("k")
					return e
				}); err != nil {
					t.Fatalf("Update: %v", err)
				}
			})
			type res struct {
				got uint64
				err error
			}
			survivor := make(chan res, 1)
			cancelled := make(chan error, 1)
			waiterBody := func(signal string, wait func(*gmdb.DB, uint64) (uint64, error)) res {
				ctx := context.Background()
				db, err := gmdb.Open(ctx, "/db", smallOpts)
				if err != nil {
					return res{0, err}
				}
				defer db.Close()
				v, err := db.Version()
				if err != nil {
					return res{0, err}
				}
				if err := os.WriteFile(signal, nil, 0o600); err != nil {
					return res{0, err}
				}
				start := time.Now()
				got, err := wait(db, v)
				if err != nil {
					return res{got, err}
				}
				if got <= v {
					return res{got, fmt.Errorf("woke with version %d, want > %d", got, v)}
				}
				// The spec's liveness bound, not wake latency: a same-word
				// movement costs a waiter AT MOST ONE 100ms sleep slice
				// beyond the publish (cross-process.md §Notification
				// region), and the publish lands ≤ ~200ms after the last
				// park plus choreography spread. 600ms of virtual time
				// comfortably covers publish + one slice + spread on any
				// schedule while still failing a waiter that slept THROUGH
				// the publish. Immediate wake latency (the futex wake
				// itself, no slice) is pinned on the un-churned path by
				// the smoke test's <100ms bound and the fork's own futex
				// tests — under cancel churn the slice re-check is the
				// spec's own conforming safety net, so this test does not
				// discriminate wake-vs-slice.
				if el := time.Since(start); el >= 600*time.Millisecond {
					return res{got, fmt.Errorf("woke after %v of virtual time — slept through the publish", el)}
				}
				return res{got, nil}
			}
			go simulation.Process("survivor", func() {
				survivor <- waiterBody("/survivor-waiting", func(db *gmdb.DB, v uint64) (uint64, error) {
					return db.WaitVersion(context.Background(), v)
				})
			})
			kwaiter := make(chan res, 1)
			go simulation.Process("kwaiter", func() {
				// The keyspace-scoped wait parks on the keyspace hash slot
				// and must be woken by the same publish (which stamps the
				// touched keyspace's word with the bumped global version).
				kwaiter <- waiterBody("/kwaiter-waiting", func(db *gmdb.DB, v uint64) (uint64, error) {
					return db.WaitKeyspaceVersion(context.Background(), "k", v)
				})
			})
			go simulation.Process("canceller", func() {
				cancelled <- func() error { // Close completes before the send
					db, err := gmdb.Open(context.Background(), "/db", smallOpts)
					if err != nil {
						return err
					}
					defer db.Close()
					v, err := db.Version()
					if err != nil {
						return err
					}
					if err := os.WriteFile("/canceller-waiting", nil, 0o600); err != nil {
						return err
					}
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					go func() {
						time.Sleep(50 * time.Millisecond)
						cancel()
					}()
					_, err = db.WaitVersion(ctx, v)
					return err
				}()
			})
			simulation.Process("publisher", func() {
				ctx := context.Background()
				mustAwait(t, "/survivor-waiting")
				mustAwait(t, "/kwaiter-waiting")
				mustAwait(t, "/canceller-waiting")
				// Both waiters are parked. Outwait the canceller's 50ms
				// cancel so its wake churns the shared words BEFORE the
				// publish — the survivor must absorb that spurious wake
				// and still catch the real one.
				time.Sleep(200 * time.Millisecond)
				db := openDB(t, "/db")
				defer db.Close()
				if err := db.Update(ctx, func(tx *gmdb.Tx) error {
					ks, e := tx.OpenKeyspace("k")
					if e != nil {
						return e
					}
					return ks.Put([]byte("published"), []byte("v"))
				}); err != nil {
					t.Fatalf("publish Update: %v", err)
				}
			})
			if err := <-cancelled; !errors.Is(err, context.Canceled) {
				t.Fatalf("cancelled waiter = %v, want context.Canceled", err)
			}
			r := <-survivor
			if r.err != nil {
				t.Fatalf("surviving waiter: %v", r.err)
			}
			kr := <-kwaiter
			if kr.err != nil {
				t.Fatalf("keyspace waiter: %v", kr.err)
			}
		})
	})
}

// The recovery-commit gate's liveness classifier (cross-process.md
// §Stale writer recovery; durability.md §Cross-process SyncMode
// interleaving): with an UNANCHORED SyncLazy commit as the latest
// meta, a contender's Open must classify the last author — dead
// author (pid-liveness) rolls back to the durable epoch, live author
// preserves the lazy tail. This is the leg where durable != latest,
// so the two gate branches are observably different.
func TestSimulationRecoveryGateDeadVsLiveAuthor(t *testing.T) {
	lazyOpts := smallOpts
	lazyOpts.SyncMode = gmdb.SyncLazy
	simulation.Test(t, 6, func(t *testing.T) {
		simulation.Host("h", simulation.HostConfig{}, func() {
			// Dead-author leg: lazy commit, idle crash, rollback.
			simulation.Process("setup-dead", func() {
				ctx := context.Background()
				db := openDB(t, "/db-dead")
				defer db.Close()
				if err := db.Update(ctx, func(tx *gmdb.Tx) error {
					ks, e := tx.CreateKeyspace("k")
					if e != nil {
						return e
					}
					return ks.Put([]byte("committed"), []byte("v"))
				}); err != nil {
					t.Fatalf("setup Update: %v", err)
				}
			})
			go simulation.Process("dead-author", func() {
				ctx := context.Background()
				db, err := gmdb.Open(ctx, "/db-dead", lazyOpts)
				if err != nil {
					t.Errorf("Open: %v", err)
					return
				}
				if err := db.Update(ctx, func(tx *gmdb.Tx) error {
					ks, e := tx.OpenKeyspace("k")
					if e != nil {
						return e
					}
					return ks.Put([]byte("lazy"), []byte("v"))
				}); err != nil {
					t.Errorf("lazy Update: %v", err)
					return
				}
				if err := os.WriteFile("/dead-author-committed", nil, 0o600); err != nil {
					t.Errorf("signal: %v", err)
					return
				}
				time.Sleep(time.Hour) // idle — grant released, record authored
				t.Error("dead-author survived its crash")
			})
			simulation.Process("contender-dead", func() {
				ctx := context.Background()
				mustAwait(t, "/dead-author-committed")
				simulation.Crash("dead-author")
				db := openDB(t, "/db-dead")
				defer db.Close()
				if err := db.View(ctx, func(rtx *gmdb.ReadTx) error {
					ks, e := rtx.OpenKeyspaceReadOnly("k")
					if e != nil {
						return e
					}
					if _, e := ks.Get([]byte("lazy")); !errors.Is(e, gmdb.ErrNotFound) {
						t.Errorf("dead author's unanchored commit survived: %v (want rollback to durable)", e)
					}
					if _, e := ks.Get([]byte("committed")); e != nil {
						t.Errorf("durable row lost: %v", e)
					}
					return nil
				}); err != nil {
					t.Fatalf("View: %v", err)
				}
				checkAllowRecoveryDebris(t, db)
			})

			// Live-author leg: identical lazy commit, author stays alive
			// and heartbeating — the tail must be PRESERVED.
			liveDone := make(chan struct{})
			go simulation.Process("live-author", func() {
				defer close(liveDone)
				ctx := context.Background()
				db, err := gmdb.Open(ctx, "/db-live", lazyOpts)
				if err != nil {
					t.Errorf("Open: %v", err)
					return
				}
				defer db.Close()
				if err := db.Update(ctx, func(tx *gmdb.Tx) error {
					ks, e := tx.CreateKeyspace("k")
					if e != nil {
						return e
					}
					return ks.Put([]byte("lazy"), []byte("v"))
				}); err != nil {
					t.Errorf("lazy Update: %v", err)
					return
				}
				if err := os.WriteFile("/live-author-committed", nil, 0o600); err != nil {
					t.Errorf("signal: %v", err)
					return
				}
				if err := awaitFile(t, "/live-leg-done"); err != nil {
					t.Error(err)
				}
			})
			simulation.Process("contender-live", func() {
				ctx := context.Background()
				mustAwait(t, "/live-author-committed")
				db := openDB(t, "/db-live")
				defer db.Close()
				if err := db.View(ctx, func(rtx *gmdb.ReadTx) error {
					ks, e := rtx.OpenKeyspaceReadOnly("k")
					if e != nil {
						return e
					}
					if _, e := ks.Get([]byte("lazy")); e != nil {
						t.Errorf("live author's commit rolled back: %v (want preserved)", e)
					}
					return nil
				}); err != nil {
					t.Fatalf("View: %v", err)
				}
				if err := os.WriteFile("/live-leg-done", nil, 0o600); err != nil {
					t.Fatalf("signal: %v", err)
				}
			})
			<-liveDone
		})
	})
}

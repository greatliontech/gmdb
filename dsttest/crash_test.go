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

// Crash suite (docs/specs/dst-testing.md §Suites): durability.md
// walked with kernel-faithful host crashes. Commits are virtually
// instant, so mid-commit power loss is constructed by composition:
// SlowDisk stretches every disk op across virtual time, and a
// seed-derived crash delay walks the cut points — different anchor
// seeds land the power loss inside different commit steps, and
// CrashTear explores the torn-page outcome set at each. The crashed
// image is then rebooted (Host re-declaration) and must satisfy the
// durability contract exactly.

// checkAllowRecoveryDebris fails on any Check finding other than the
// spec'd post-recovery debris codes (leaked pages + the matching
// free-count drift — background leak reclaim owns both). RPL* warning
// codes are deliberately NOT allowed: these workloads never run
// reclamation, so a torn-RPL warning here would be a real fault;
// revisit when a suite adds reclamation pressure.
func checkAllowRecoveryDebris(t *testing.T, db *gmdb.DB) {
	t.Helper()
	for issue := range db.Check() {
		if issue.Code == "BitmapLeak" || issue.Code == "FreeCountMismatch" {
			continue
		}
		t.Errorf("Check: %+v", issue)
	}
}

// Acked-durable commits survive power loss at ANY cut point, torn
// pages included; an in-flight commit is atomic — after reboot it is
// either fully present or fully absent, and the prior epoch is intact
// (durability.md §Recovery, §Commit Outcome Classification's acked
// class).
func TestSimulationHostCrashMidCommitAtomicity(t *testing.T) {
	// Anchor (seed, crash-delay) pairs walking the stretched commit:
	// under SlowDisk(2ms) the 8-row commit spans virtual ~0-22ms with
	// the publish tail (data fsync -> meta pwrite -> meta fsync) at
	// ~18-22ms, and the writer's ack marker is durable by ~28ms. The
	// set straddles all three regions - instrumented probes confirmed
	// in-commit crashes (present=0), a tail unsynced-meta CrashTear
	// draw (present=8 with the ack never issued), AND post-ack crashes
	// where the durable marker forces present==8.
	for _, anchor := range []struct {
		seed  uint64
		delay time.Duration
	}{{1, 5}, {2, 8}, {3, 11}, {4, 14}, {5, 17}, {6, 20}, {13, 21}, {20, 35}, {21, 40}} {
		seed := anchor.seed
		simulation.TestWith(t, seed, simulation.Options{CrashTear: true}, func(t *testing.T) {
			crashDelay := anchor.delay * time.Millisecond
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
						for i := range 4 {
							if e := ks.Put(fmt.Appendf(nil, "acked-%d", i), []byte("v")); e != nil {
								return e
							}
						}
						return nil
					}); err != nil {
						t.Fatalf("setup Update: %v", err)
					}
				})
				go simulation.Process("writer", func() {
					ctx := context.Background()
					db, err := gmdb.Open(ctx, "/db", smallOpts)
					if err != nil {
						t.Errorf("Open: %v", err)
						return
					}
					simulation.SlowDisk("h", 2*time.Millisecond)
					// The in-flight commit the power loss lands inside.
					err = db.Update(ctx, func(tx *gmdb.Tx) error {
						ks, e := tx.OpenKeyspace("k")
						if e != nil {
							return e
						}
						for i := range 8 {
							v := make([]byte, 900)
							v[0] = byte('A' + i)
							if e := ks.Put(fmt.Appendf(nil, "inflight-%d", i), v); e != nil {
								return e
							}
						}
						return nil
					})
					if err == nil {
						// Acked. Durably record the ack so recovery can
						// demand the commit: marker-durable ⇒ ack happened
						// ⇒ the rows MUST be there (a lost acked commit
						// must not pass as "atomically absent").
						if f, e := os.OpenFile("/acked", os.O_CREATE|os.O_WRONLY, 0o600); e == nil {
							_ = f.Sync()
							_ = f.Close()
							if d, e := os.Open("/"); e == nil {
								_ = d.Sync()
								_ = d.Close()
							}
						}
					}
					// The host dies under this sleep either way.
					time.Sleep(time.Hour)
				})
				simulation.Process("chaos", func() {
					time.Sleep(crashDelay)
				})
			})
			simulation.CrashHost("h")
			simulation.Host("h", simulation.HostConfig{}, func() {
				simulation.Process("recovery", func() {
					ctx := context.Background()
					db := openDB(t, "/db")
					defer db.Close()
					checkAllowRecoveryDebris(t, db)
					if err := db.View(ctx, func(rtx *gmdb.ReadTx) error {
						ks, e := rtx.OpenKeyspaceReadOnly("k")
						if e != nil {
							return e
						}
						for i := range 4 {
							if _, e := ks.Get(fmt.Appendf(nil, "acked-%d", i)); e != nil {
								t.Errorf("seed %d: acked row %d lost after power loss: %v", seed, i, e)
							}
						}
						// Commit atomicity: the in-flight rows are all
						// present (byte-exact) or all absent — a mix is a
						// torn commit made visible.
						present := 0
						for i := range 8 {
							v, e := ks.Get(fmt.Appendf(nil, "inflight-%d", i))
							switch {
							case e == nil:
								if len(v) != 900 || v[0] != byte('A'+i) {
									t.Errorf("seed %d: inflight row %d bytes drifted", seed, i)
								}
								present++
							case errors.Is(e, gmdb.ErrNotFound):
							default:
								t.Errorf("seed %d: inflight row %d read error: %v", seed, i, e)
							}
						}
						if present != 0 && present != 8 {
							t.Errorf("seed %d: torn commit visible — %d/8 in-flight rows present", seed, present)
						}
						if _, e := os.Lstat("/acked"); e == nil && present != 8 {
							t.Errorf("seed %d: durably ACKED commit lost (%d/8 rows) — the ack marker survived, the data did not", seed, present)
						}
						return nil
					}); err != nil {
						t.Fatalf("seed %d: View: %v", seed, err)
					}
					// The torn lock file must not wedge coordination: the
					// power-lost author's record classifies dead and a
					// fresh write goes through.
					if err := db.Update(ctx, func(tx *gmdb.Tx) error {
						ks, e := tx.OpenKeyspace("k")
						if e != nil {
							return e
						}
						return ks.Put([]byte("post-reboot"), []byte("v"))
					}); err != nil {
						t.Fatalf("seed %d: post-reboot Update: %v", seed, err)
					}
				})
			})
		})
	}
}

// SyncLazy commits die with the page cache: after power loss the
// database recovers to the DURABLE epoch — the lazy tail is gone
// regardless of author liveness (durability.md §Durability Modes;
// the process-crash counterpart, where the tail SURVIVES, is
// TestSimulationRecoveryGateDeadVsLiveAuthor's live leg).
func TestSimulationHostCrashDropsLazyTail(t *testing.T) {
	lazyOpts := smallOpts
	lazyOpts.SyncMode = gmdb.SyncLazy
	simulation.TestWith(t, 7, simulation.Options{CrashTear: true}, func(t *testing.T) {
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
					return ks.Put([]byte("durable"), []byte("v"))
				}); err != nil {
					t.Fatalf("setup Update: %v", err)
				}
			})
			go simulation.Process("lazy-writer", func() {
				ctx := context.Background()
				db, err := gmdb.Open(ctx, "/db", lazyOpts)
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
				time.Sleep(time.Hour) // alive at power loss — liveness must not matter
			})
			simulation.Process("spacer", func() {
				time.Sleep(50 * time.Millisecond) // lazy commit completes first
			})
		})
		simulation.CrashHost("h")
		simulation.Host("h", simulation.HostConfig{}, func() {
			simulation.Process("recovery", func() {
				ctx := context.Background()
				db := openDB(t, "/db")
				defer db.Close()
				checkAllowRecoveryDebris(t, db)
				if err := db.View(ctx, func(rtx *gmdb.ReadTx) error {
					ks, e := rtx.OpenKeyspaceReadOnly("k")
					if e != nil {
						return e
					}
					if _, e := ks.Get([]byte("durable")); e != nil {
						t.Errorf("durable row lost: %v", e)
					}
					if _, e := ks.Get([]byte("lazy")); !errors.Is(e, gmdb.ErrNotFound) {
						t.Errorf("lazy tail survived power loss: %v (want rollback to the durable epoch)", e)
					}
					return nil
				}); err != nil {
					t.Fatalf("View: %v", err)
				}
			})
		})
	})
}

// CopyTo's destination crash-consistency invariant under power loss
// (api-surface.md §Check, CopyTo, Compact): with the publish
// stretched by SlowDisk and the crash landing at seed-dependent cut
// points, the destination path after reboot either names a COMPLETE,
// openable copy or nothing — never a partial file (the inert temp may
// remain).
func TestSimulationHostCrashMidCopyToPublish(t *testing.T) {
	for _, seed := range []uint64{8, 9, 10, 11} {
		simulation.TestWith(t, seed, simulation.Options{CrashTear: true}, func(t *testing.T) {
			crashDelay := time.Duration(5+(seed*11)%50) * time.Millisecond
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
						for i := range 16 {
							if e := ks.Put(fmt.Appendf(nil, "row-%02d", i), make([]byte, 400)); e != nil {
								return e
							}
						}
						return nil
					}); err != nil {
						t.Fatalf("setup Update: %v", err)
					}
				})
				go simulation.Process("copier", func() {
					ctx := context.Background()
					db, err := gmdb.Open(ctx, "/db", smallOpts)
					if err != nil {
						t.Errorf("Open: %v", err)
						return
					}
					simulation.SlowDisk("h", 2*time.Millisecond)
					_ = db.CopyTo("/backup.db", false) // the crash may land anywhere inside
					time.Sleep(time.Hour)
				})
				simulation.Process("chaos", func() {
					time.Sleep(crashDelay)
				})
			})
			simulation.CrashHost("h")
			simulation.Host("h", simulation.HostConfig{}, func() {
				simulation.Process("recovery", func() {
					ctx := context.Background()
					// The source must always recover.
					db := openDB(t, "/db")
					checkAllowRecoveryDebris(t, db)
					db.Close()
					// The destination: complete copy or nothing. Probe
					// existence first — Open would CREATE an empty database
					// at an absent path.
					if _, err := os.Lstat("/backup.db"); err != nil {
						return // nothing published — conforming
					}
					bdb, err := gmdb.Open(ctx, "/backup.db", smallOpts)
					if err != nil {
						t.Errorf("seed %d: backup path names a file that does not open: %v", seed, err)
						return
					}
					defer bdb.Close()
					n := 0
					if err := bdb.View(ctx, func(rtx *gmdb.ReadTx) error {
						ks, e := rtx.OpenKeyspaceReadOnly("k")
						if e != nil {
							return e
						}
						for range ks.All() {
							n++
						}
						return ks.Err()
					}); err != nil {
						t.Errorf("seed %d: published backup unreadable: %v", seed, err)
						return
					}
					if n != 16 {
						t.Errorf("seed %d: published backup has %d/16 rows — a partial copy at path", seed, n)
					}
					for issue := range bdb.Check() {
						t.Errorf("seed %d: backup Check: %+v", seed, issue)
					}
				})
			})
		})
	}
}

// SyncDataOnly loses AT MOST the last commit after power loss
// (durability.md §Durability Modes): of two sequential acked commits,
// the first must survive; the second may fall back to the surviving
// meta's sub-record. Each surviving commit is whole.
func TestSimulationHostCrashSyncDataOnlyEpoch(t *testing.T) {
	sdOpts := smallOpts
	sdOpts.SyncMode = gmdb.SyncDataOnly
	simulation.TestWith(t, 14, simulation.Options{CrashTear: true}, func(t *testing.T) {
		simulation.Host("h", simulation.HostConfig{}, func() {
			go simulation.Process("writer", func() {
				ctx := context.Background()
				db, err := gmdb.Open(ctx, "/db", sdOpts)
				if err != nil {
					t.Errorf("Open: %v", err)
					return
				}
				for i, key := range []string{"first", "second"} {
					if err := db.Update(ctx, func(tx *gmdb.Tx) error {
						ks, e := tx.CreateKeyspace("k")
						if e != nil {
							if ks, e = tx.OpenKeyspace("k"); e != nil {
								return e
							}
						}
						return ks.Put([]byte(key), []byte("v"))
					}); err != nil {
						t.Errorf("Update %d: %v", i, err)
						return
					}
				}
				time.Sleep(time.Hour) // both acked; power loss lands here
			})
			simulation.Process("spacer", func() {
				time.Sleep(50 * time.Millisecond)
			})
		})
		simulation.CrashHost("h")
		simulation.Host("h", simulation.HostConfig{}, func() {
			simulation.Process("recovery", func() {
				ctx := context.Background()
				db := openDB(t, "/db")
				defer db.Close()
				checkAllowRecoveryDebris(t, db)
				if err := db.View(ctx, func(rtx *gmdb.ReadTx) error {
					ks, e := rtx.OpenKeyspaceReadOnly("k")
					if e != nil {
						return e
					}
					if _, e := ks.Get([]byte("first")); e != nil {
						t.Errorf("first SyncDataOnly commit lost: %v (at most the LAST may be lost)", e)
					}
					if _, e := ks.Get([]byte("second")); e != nil && !errors.Is(e, gmdb.ErrNotFound) {
						t.Errorf("second commit read error: %v", e)
					}
					return nil
				}); err != nil {
					t.Fatalf("View: %v", err)
				}
			})
		})
	})
}

// Compact's in-place publish under power loss (durability.md
// §Directory-entry durability, Compact leg): whatever cut point the
// crash lands on, the SOURCE path must reopen with every row intact —
// the old file or the fully-published rebuild, never a hybrid.
func TestSimulationHostCrashMidCompact(t *testing.T) {
	// The stretched Compact spans virtual ~0-52ms with the in-place
	// rename+dir-fsync publish at ~48-52ms; the anchors sample early
	// rebuild, mid-rebuild, the publish window, and post-completion.
	for _, anchor := range []struct {
		seed  uint64
		delay time.Duration
	}{{15, 5}, {16, 23}, {17, 45}, {18, 50}, {19, 56}} {
		seed := anchor.seed
		simulation.TestWith(t, seed, simulation.Options{CrashTear: true}, func(t *testing.T) {
			crashDelay := anchor.delay * time.Millisecond
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
						for i := range 16 {
							if e := ks.Put(fmt.Appendf(nil, "row-%02d", i), make([]byte, 400)); e != nil {
								return e
							}
						}
						return nil
					}); err != nil {
						t.Fatalf("setup Update: %v", err)
					}
				})
				go simulation.Process("compactor", func() {
					db, err := gmdb.Open(context.Background(), "/db", smallOpts)
					if err != nil {
						t.Errorf("Open: %v", err)
						return
					}
					simulation.SlowDisk("h", 2*time.Millisecond)
					_ = db.Compact()
					time.Sleep(time.Hour)
				})
				simulation.Process("chaos", func() {
					time.Sleep(crashDelay)
				})
			})
			simulation.CrashHost("h")
			simulation.Host("h", simulation.HostConfig{}, func() {
				simulation.Process("recovery", func() {
					ctx := context.Background()
					db := openDB(t, "/db")
					defer db.Close()
					checkAllowRecoveryDebris(t, db)
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
						t.Fatalf("seed %d: View: %v", seed, err)
					}
					if n != 16 {
						t.Errorf("seed %d: %d/16 rows after mid-compact power loss", seed, n)
					}
				})
			})
		})
	}
}

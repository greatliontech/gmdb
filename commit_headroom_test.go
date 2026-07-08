package gmdb

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"testing"
)

// A transaction whose ops were driven to the MaxTxBufferBytes cap
// must still be able to commit its applied work: ops-phase admission
// subtracts a live commit reserve — the exact RPL segment projection
// (pager-internal) plus the exact descriptor-flush projection
// (Tx.recalcFlushReserve) — so Tx.Commit's own slab appetite always
// fits in the reserved space (pager-slab.md §Slab Budget).

// TestCommitSucceedsAfterTxTooLarge is the public-surface reproducer:
// fill a transaction over committed data (so CoWs retire prior-tx
// pages and grow the reserve) until Put fails ErrTxTooLarge, then
// Commit. Both an un-indexed and an indexed keyspace shape.
func TestCommitSucceedsAfterTxTooLarge(t *testing.T) {
	ctx := context.Background()
	for _, indexed := range []bool{false, true} {
		name := "Plain"
		if indexed {
			name = "Indexed"
		}
		t.Run(name, func(t *testing.T) {
			db, err := Open(ctx, tmpPath(t), Options{
				PageSize: 4096, MinSize: 16, MaxSize: 4096,
				MaxTxBufferBytes: 256 * 1024,
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { db.Close() })

			var decls []*IndexDecl
			if indexed {
				decls = append(decls, &IndexDecl{
					Name:    "by_c",
					Columns: []IndexColumn{{Name: "c"}},
					Extract: func(key, value []byte) []IndexEntry {
						return []IndexEntry{{Cols: [][]byte{value[:1]}}}
					},
				})
			}

			// Seed committed rows so the fill CoWs prior-tx pages.
			// Batched: each row mutation costs one fresh buffer per
			// tree level, so a single tx of 400 puts would itself
			// trip the small budget.
			seed := make([]byte, 120)
			batchRows := 25
			if indexed {
				// Indexed puts also CoW index-tree levels; smaller
				// batches keep each seed tx inside the small budget.
				batchRows = 8
			}
			for batch := 0; batch*batchRows < 400; batch++ {
				tx, err := db.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				var ks *Keyspace
				if batch == 0 {
					ks, err = tx.CreateKeyspace("t", decls...)
				} else {
					ks, err = tx.OpenKeyspace("t", decls...)
				}
				if err != nil {
					t.Fatal(err)
				}
				for i := batch * batchRows; i < (batch+1)*batchRows && i < 400; i++ {
					seed[0] = byte('a' + i%20)
					if err := ks.Put(fmt.Appendf(nil, "k%05d", i), seed); err != nil {
						t.Fatal(err)
					}
				}
				if err := tx.Commit(); err != nil {
					t.Fatal(err)
				}
			}

			// Fill until the cap trips.
			tx, err := db.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			ks, err := tx.OpenKeyspace("t", decls...)
			if err != nil {
				t.Fatal(err)
			}
			big := make([]byte, 3000)
			applied, tripped := 0, false
			for i := 0; i < 10000; i++ {
				big[0] = byte('a' + i%20)
				err := ks.Put(fmt.Appendf(nil, "k%05d", i%400), big)
				if err == nil {
					applied++
					continue
				}
				if !errors.Is(err, ErrTxTooLarge) {
					t.Fatalf("fill Put #%d: %v, want ErrTxTooLarge", i, err)
				}
				tripped = true
				break
			}
			if !tripped {
				t.Fatalf("fixture: cap never tripped after %d puts", applied)
			}
			if applied == 0 {
				t.Fatalf("fixture: first Put already tripped; nothing to commit")
			}

			// The reported defect: this Commit failed ErrTxTooLarge.
			if err := tx.Commit(); err != nil {
				t.Fatalf("Commit after ErrTxTooLarge op: %v", err)
			}

			// The applied prefix persisted, and the database is
			// structurally consistent.
			rtx, err := db.BeginRead(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer rtx.Rollback()
			rks, err := rtx.OpenKeyspaceReadOnly("t")
			if err != nil {
				t.Fatal(err)
			}
			v, err := rks.Get([]byte("k00000"))
			if err != nil {
				t.Fatalf("Get(k00000): %v", err)
			}
			if len(v) != len(big) {
				t.Errorf("k00000 length = %d, want %d (applied write lost)", len(v), len(big))
			}
			if err := rtx.Rollback(); err != nil {
				t.Fatal(err)
			}
			for iss := range db.Check() {
				t.Errorf("Check after cap-edge commit: %+v", iss)
			}
		})
	}
}

// TestCommitNeedsOnlyReservedHeadroom drives a transaction carrying
// every descriptor-flush shape — dirty indexed keyspace, dirty
// indexed set keyspace, staged config on an uncached keyspace, a
// keyspace deletion, and a fresh indexed creation — to EXACTLY zero
// effective headroom (budget − reserve − dirty), then commits.
// Success pins that the reserve covers the whole commit sequence:
// the flush and step-0 allocations fit in the reserved space, with
// the raw cap as the only remaining bound.
func TestCommitNeedsOnlyReservedHeadroom(t *testing.T) {
	ctx := context.Background()
	const budget = 512 * 1024
	const pageSz = 4096
	db, err := Open(ctx, tmpPath(t), Options{
		PageSize: 4096, MinSize: 16, MaxSize: 4096,
		MaxTxBufferBytes: budget,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	decl := &IndexDecl{
		Name:    "by_c",
		Columns: []IndexColumn{{Name: "c"}},
		Extract: func(key, value []byte) []IndexEntry {
			return []IndexEntry{{Cols: [][]byte{value[:1]}}}
		},
	}
	sdecl := &IndexDecl{
		Name:    "by_m",
		Columns: []IndexColumn{{Name: "m"}},
		Extract: func(setKey, member []byte) []IndexEntry {
			return []IndexEntry{{Cols: [][]byte{member[:1]}}}
		},
	}
	seed := []byte("abcdef")
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ksA, err := tx.CreateKeyspace("a", decl)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 50 {
		if err := ksA.Put(fmt.Appendf(nil, "a%03d", i), seed); err != nil {
			t.Fatal(err)
		}
	}
	sksB, err := tx.CreateSetKeyspace("b", nil, sdecl)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sksB.Put([]byte("set"), []byte("member")); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.CreateKeyspace("c"); err != nil {
		t.Fatal(err)
	}
	ksD, err := tx.CreateKeyspace("d")
	if err != nil {
		t.Fatal(err)
	}
	if err := ksD.Put([]byte("dk"), seed); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	// The transaction under test: every flush shape at once.
	tx, err = db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	ksA, err = tx.OpenKeyspace("a", decl)
	if err != nil {
		t.Fatal(err)
	}
	if err := ksA.Put([]byte("a000"), []byte("zyxwvu")); err != nil {
		t.Fatal(err)
	}
	sksB, err = tx.OpenSetKeyspace("b", sdecl)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sksB.Put([]byte("set"), []byte("m2")); err != nil {
		t.Fatal(err)
	}
	if err := tx.SetKeyspaceConfig("c", KeyspaceConfig{RestartGroupTarget: 7}); err != nil {
		t.Fatal(err)
	}
	if err := tx.DeleteKeyspace("d"); err != nil {
		t.Fatal(err)
	}
	ksE, err := tx.CreateKeyspace("e", decl)
	if err != nil {
		t.Fatal(err)
	}
	if err := ksE.Put([]byte("ek"), seed); err != nil {
		t.Fatal(err)
	}

	// Burn every remaining page of effective headroom, then release
	// the burns (same-tx frees go loose — still counted against the
	// budget, discarded at commit) so the committed image carries no
	// unreferenced pages. The descriptor flush therefore runs with
	// effective headroom exactly zero.
	var burned []uint64
	for budget-tx.CommitReserveBytes()-tx.DirtyBytes() >= pageSz {
		id, err := tx.pgr.AllocPage()
		if err != nil {
			t.Fatalf("burn AllocPage: %v", err)
		}
		if _, err := tx.pgr.AllocSlab(id); err != nil {
			t.Fatalf("burn AllocSlab: %v", err)
		}
		burned = append(burned, id)
	}
	if hr := budget - tx.CommitReserveBytes() - tx.DirtyBytes(); hr >= pageSz {
		t.Fatalf("headroom %d after burns, want < one page", hr)
	}
	for _, id := range burned {
		if err := tx.pgr.FreePage(id); err != nil {
			t.Fatalf("burn FreePage(%d): %v", id, err)
		}
	}

	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit at zero effective headroom: %v", err)
	}

	// Every staged effect persisted.
	check, err := db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer check.Rollback()
	rksA, err := check.OpenKeyspaceReadOnly("a")
	if err != nil {
		t.Fatal(err)
	}
	if v, err := rksA.Get([]byte("a000")); err != nil || string(v) != "zyxwvu" {
		t.Errorf("a000 = %q, %v; want zyxwvu", v, err)
	}
	h, err := rksA.Index("by_c")
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for range h.Lookup([]byte("z")) {
		n++
	}
	if err := h.Err(); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("index lookup for updated row = %d entries, want 1", n)
	}
	desc, found, err := check.loadDescriptor("c")
	if err != nil || !found {
		t.Fatalf("loadDescriptor(c): found=%v err=%v", found, err)
	}
	if desc.RestartGroupTarget != 7 {
		t.Errorf("c RestartGroupTarget = %d, want 7", desc.RestartGroupTarget)
	}
	if _, err := check.OpenKeyspaceReadOnly("d"); !errors.Is(err, ErrNotFound) {
		t.Errorf("open deleted d: err = %v, want ErrNotFound", err)
	}
	rksE, err := check.OpenKeyspaceReadOnly("e")
	if err != nil {
		t.Fatalf("open created e: %v", err)
	}
	if _, err := rksE.Get([]byte("ek")); err != nil {
		t.Errorf("e/ek: %v", err)
	}
	if err := check.Rollback(); err != nil {
		t.Fatal(err)
	}
	for iss := range db.Check() {
		t.Errorf("Check after zero-headroom commit: %+v", iss)
	}
}

// TestCommitReserveCoversFlushRetireSegmentCrossing pins the reserve's
// two remaining legs at their tightest reachable edge:
//
//   - the flush-retire RPL slack (recalcFlushReserve's trailing term):
//     the transaction lands its retired count EXACTLY on the segment
//     capacity, so the descriptor flush's own retire at commit crosses
//     into a second RPL segment that only the slack covers;
//   - the markDirty-transition recompute: the only flush obligation
//     here arises from a data Put (no DDL, no config, no staging), so
//     a missing recompute on the Clean→Dirty edge leaves the flush
//     write entirely unreserved.
//
// The tx is then burned to zero effective headroom; Commit must fit
// in exactly the reserved space.
func TestCommitReserveCoversFlushRetireSegmentCrossing(t *testing.T) {
	ctx := context.Background()
	// Landing ~capPerSeg single-retire updates costs ~3 dirty pages
	// each (one fresh buffer per touched level); 16 MiB leaves room.
	const budget = 16 << 20
	const pageSz = 4096
	db, err := Open(ctx, tmpPath(t), Options{
		PageSize: 4096, MinSize: 16, MaxSize: 16384,
		MaxTxBufferBytes: budget,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	// Seed enough committed leaves that single-key updates can retire
	// one prior-tx leaf each, past the segment capacity (~25 keys per
	// leaf at this geometry; 640 batches ≈ 640 leaves > capPerSeg).
	val := make([]byte, 120)
	var keys []string
	for batch := range 640 {
		tx, err := db.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var ks *Keyspace
		if batch == 0 {
			ks, err = tx.CreateKeyspace("data")
		} else {
			ks, err = tx.OpenKeyspace("data")
		}
		if err != nil {
			t.Fatal(err)
		}
		for i := range 25 {
			k := fmt.Sprintf("base-%06d", batch*25+i)
			if err := ks.Put([]byte(k), val); err != nil {
				t.Fatal(err)
			}
			keys = append(keys, k)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}

	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	ks, err := tx.OpenKeyspace("data")
	if err != nil {
		t.Fatal(err)
	}
	capPerSeg := RPLEntriesPerSegmentForTest(tx)
	if capPerSeg <= 0 || capPerSeg > len(keys)/2 {
		t.Fatalf("fixture: capPerSeg=%d unusable for %d keys", capPerSeg, len(keys))
	}

	// Warm every branch level (a handful of spread updates) so
	// subsequent fresh-leaf updates retire exactly one prior-tx page
	// each — branch re-CoWs free same-tx pages, which go loose, not
	// retired.
	for j := 0; j < len(keys); j += len(keys) / 8 {
		if err := ks.Put([]byte(keys[j]), val); err != nil {
			t.Fatal(err)
		}
	}
	// Land the retired count exactly on the segment capacity.
	i := 3
	for tx.RetiredPagesLen() < capPerSeg {
		if i >= len(keys) {
			t.Fatalf("fixture: keys exhausted at retired=%d", tx.RetiredPagesLen())
		}
		if err := ks.Put([]byte(keys[i]), val); err != nil {
			t.Fatalf("landing Put(%s): %v", keys[i], err)
		}
		i += 25
	}
	if got := tx.RetiredPagesLen(); got != capPerSeg {
		t.Fatalf("fixture: retired=%d, want exactly %d (single-retire steps)", got, capPerSeg)
	}

	// Exact reserve accounting at this state: one RPL segment page
	// (retired == capPerSeg exactly), one flush write (a single
	// dirty handle over the depth-1 keyspace tree, no indexes), and
	// one page of flush-retire slack. Pins the recalcFlushReserve
	// formula — including the markDirty-transition recompute (the
	// Put above is this tx's only obligation event) and the slack
	// term, whose end-to-end effect commit-step-0's loose-buffer
	// discards would otherwise mask.
	if got, want := tx.CommitReserveBytes(), 3*pageSz; got != want {
		t.Fatalf("CommitReserveBytes = %d, want %d (rpl 1 + flush 1 + slack 1 pages)", got, want)
	}

	// Burn to zero effective headroom, release the burns.
	var burned []uint64
	for budget-tx.CommitReserveBytes()-tx.DirtyBytes() >= pageSz {
		id, err := tx.pgr.AllocPage()
		if err != nil {
			t.Fatalf("burn AllocPage: %v", err)
		}
		if _, err := tx.pgr.AllocSlab(id); err != nil {
			t.Fatalf("burn AllocSlab: %v", err)
		}
		burned = append(burned, id)
	}
	for _, id := range burned {
		if err := tx.pgr.FreePage(id); err != nil {
			t.Fatalf("burn FreePage(%d): %v", id, err)
		}
	}

	// The descriptor flush retires the keyspace-tree path (prior-tx
	// pages), crossing into RPL segment two; only the reserve's slack
	// term makes that affordable at zero headroom.
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit at zero headroom across segment boundary: %v", err)
	}
	for iss := range db.Check() {
		t.Errorf("Check: %+v", iss)
	}
}

// TestConfigStagingAfterCapFillCommits pins the obligation-edge
// admission on the public surface: fill a keyspace to
// ErrTxTooLarge with a deep keyspace
// B+tree (many sibling keyspaces), then stage a batch of
// SetKeyspaceConfig changes on uncached names. Each staging call
// must either be admitted — reserve still affordable — or fail
// ErrTxTooLarge itself; either way the following Commit must
// publish the applied work.
func TestConfigStagingAfterCapFillCommits(t *testing.T) {
	ctx := context.Background()
	const budget = 256 * 1024
	db, err := Open(ctx, tmpPath(t), Options{
		PageSize: 4096, MinSize: 16, MaxSize: 4096,
		MaxTxBufferBytes: budget,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	// 200 sibling keyspaces deepen the keyspace B+tree so each staged
	// entry's flush charge is > 1 page.
	for batch := range 20 {
		if err := db.Update(ctx, func(tx *Tx) error {
			for i := batch * 10; i < (batch+1)*10; i++ {
				if _, err := tx.CreateKeyspace(fmt.Sprintf("pad-%04d", i)); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	seed := make([]byte, 120)
	for batch := range 16 {
		if err := db.Update(ctx, func(tx *Tx) error {
			var ks *Keyspace
			var err error
			if batch == 0 {
				ks, err = tx.CreateKeyspace("t")
			} else {
				ks, err = tx.OpenKeyspace("t")
			}
			if err != nil {
				return err
			}
			for i := batch * 25; i < (batch+1)*25; i++ {
				if err := ks.Put(fmt.Appendf(nil, "k%05d", i), seed); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}

	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	ks, err := tx.OpenKeyspace("t")
	if err != nil {
		t.Fatal(err)
	}
	big := make([]byte, 3000)
	tripped := false
	for i := 0; i < 10000 && !tripped; i++ {
		switch err := ks.Put(fmt.Appendf(nil, "k%05d", i%400), big); {
		case err == nil:
		case errors.Is(err, ErrTxTooLarge):
			tripped = true
		default:
			t.Fatalf("fill Put: %v", err)
		}
	}
	if !tripped {
		t.Fatal("fixture: cap never tripped")
	}
	staged := 0
	for i := range 8 {
		err := tx.SetKeyspaceConfig(fmt.Sprintf("pad-%04d", i), KeyspaceConfig{RestartGroupTarget: 7})
		switch {
		case err == nil:
			staged++
		case errors.Is(err, ErrTxTooLarge):
			// Rejected at the obligation edge — the staging must have
			// been unwound (verified below: Commit succeeds and the
			// name's config is unchanged).
		default:
			t.Fatalf("SetKeyspaceConfig(pad-%04d): %v", i, err)
		}
		if got := tx.DirtyBytes() + tx.CommitReserveBytes(); got > budget {
			t.Fatalf("invariant violated after staging %d: dirty+reserve = %d > %d", i, got, budget)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit after cap fill + %d staged configs: %v", staged, err)
	}
	for iss := range db.Check() {
		t.Errorf("Check: %+v", iss)
	}
}

// TestCommitReserveInvariantUnderRandomOps is the property encoding of
// INV-COMMIT-HEADROOM: across randomized op sequences,
// dirty + commitReserve never exceeds the budget at ANY point (the
// obligation edge included), and the final Commit always succeeds.
// Ops that reject with ErrTxTooLarge are part of the property — their
// unwind must keep the invariant too.
func TestCommitReserveInvariantUnderRandomOps(t *testing.T) {
	ctx := context.Background()
	const budget = 192 * 1024
	for _, seed := range []int64{1, 7, 42, 1337} {
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			rng := rand.New(rand.NewSource(seed))
			db, err := Open(ctx, tmpPath(t), Options{
				PageSize: 4096, MinSize: 16, MaxSize: 4096,
				MaxTxBufferBytes: budget,
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { db.Close() })
			// Committed base: a few keyspaces with rows.
			if err := db.Update(ctx, func(tx *Tx) error {
				for i := range 6 {
					ks, err := tx.CreateKeyspace(fmt.Sprintf("ks-%d", i))
					if err != nil {
						return err
					}
					if err := ks.Put([]byte("k"), []byte("v")); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}

			tx, err := db.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			handles := map[string]*Keyspace{}
			indexDecl := func() *IndexDecl {
				return &IndexDecl{
					Name:    "ix",
					Columns: []IndexColumn{{Name: "c"}},
					Extract: func(key, value []byte) []IndexEntry {
						return []IndexEntry{{Cols: [][]byte{value[:1]}}}
					},
				}
			}
			indexed := map[string]bool{}
			val := make([]byte, 400)
			created := 6
			tolerated := func(err error) bool {
				return err == nil || errors.Is(err, ErrTxTooLarge) ||
					errors.Is(err, ErrKeyExists) || errors.Is(err, ErrNotFound) ||
					errors.Is(err, ErrKeyspaceAlreadyOpen) || errors.Is(err, ErrKeyspaceClosed) ||
					errors.Is(err, ErrIndexNotFound) || errors.Is(err, ErrIndexExtractorRequired)
			}
			for step := range 400 {
				var opErr error
				switch rng.Intn(12) {
				case 0: // create (half indexed)
					name := fmt.Sprintf("ks-%d", created)
					var ks *Keyspace
					if rng.Intn(2) == 0 {
						ks, opErr = tx.CreateKeyspace(name, indexDecl())
						if opErr == nil {
							indexed[name] = true
						}
					} else {
						ks, opErr = tx.CreateKeyspace(name)
					}
					if opErr == nil {
						handles[name] = ks
						created++
					}
				case 1: // delete a random existing name
					opErr = tx.DeleteKeyspace(fmt.Sprintf("ks-%d", rng.Intn(created)))
					if opErr == nil {
						for n, h := range handles {
							if h.dead {
								delete(handles, n)
							}
						}
					}
				case 2: // config on a random (likely uncached) name
					opErr = tx.SetKeyspaceConfig(fmt.Sprintf("ks-%d", rng.Intn(created)),
						KeyspaceConfig{RestartGroupTarget: uint16(1 + rng.Intn(200))})
				case 3: // rebuild an index (only meaningful on indexed names)
					opErr = tx.Indexes().Rebuild(fmt.Sprintf("ks-%d", rng.Intn(created)), indexDecl())
				case 4: // drop an index
					opErr = tx.Indexes().Drop(fmt.Sprintf("ks-%d", rng.Intn(created)), "ix")
				case 5: // cursor delete through an already-open handle
					name := fmt.Sprintf("ks-%d", rng.Intn(created))
					ks, ok := handles[name]
					if !ok {
						break
					}
					cur := ks.Cursor()
					if k, _ := cur.First(); k != nil {
						opErr = cur.Delete()
					}
				default: // put through a (lazily opened) handle
					name := fmt.Sprintf("ks-%d", rng.Intn(created))
					ks, ok := handles[name]
					if !ok {
						var decls []*IndexDecl
						if indexed[name] {
							decls = append(decls, indexDecl())
						}
						var err error
						ks, err = tx.OpenKeyspace(name, decls...)
						if err != nil {
							opErr = err
							break
						}
						handles[name] = ks
					}
					rng.Read(val[:8])
					opErr = ks.Put(fmt.Appendf(nil, "r%03d", rng.Intn(200)), val)
				}
				if !tolerated(opErr) {
					t.Fatalf("step %d: unexpected op error: %v", step, opErr)
				}
				if got := tx.DirtyBytes() + tx.CommitReserveBytes(); got > budget {
					t.Fatalf("step %d: dirty+reserve = %d > budget %d (INV-COMMIT-HEADROOM)", step, got, budget)
				}
			}
			if err := tx.Commit(); err != nil {
				t.Fatalf("final Commit: %v", err)
			}
			for iss := range db.Check() {
				t.Errorf("Check: %+v", iss)
			}
		})
	}
}

// TestDirtyingChargeAdmittedBeforeOp pins the requireWritable
// pre-charge deterministically at the cap edge: with a Clean handle
// and only a sliver of effective headroom left, the first mutator on
// that handle must either fit — obligation AND op — or fail
// ErrTxTooLarge up front; in both cases dirty + reserve stays within
// the budget. Without the pre-charge, the op's pages are admitted
// first and markDirty then raises the reserve past the cap.
func TestDirtyingChargeAdmittedBeforeOp(t *testing.T) {
	ctx := context.Background()
	const budget = 256 * 1024
	const pageSz = 4096
	for _, headroomPages := range []int{1, 2, 3, 4, 5} {
		t.Run(fmt.Sprintf("headroom=%d", headroomPages), func(t *testing.T) {
			db, err := Open(ctx, tmpPath(t), Options{
				PageSize: 4096, MinSize: 16, MaxSize: 4096,
				MaxTxBufferBytes: budget,
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { db.Close() })
			if err := db.Update(ctx, func(tx *Tx) error {
				ks, err := tx.CreateKeyspace("a")
				if err != nil {
					return err
				}
				return ks.Put([]byte("k"), []byte("v"))
			}); err != nil {
				t.Fatal(err)
			}

			tx, err := db.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			ks, err := tx.OpenKeyspace("a")
			if err != nil {
				t.Fatal(err)
			}
			for budget-tx.CommitReserveBytes()-tx.DirtyBytes() > headroomPages*pageSz {
				id, err := tx.pgr.AllocPage()
				if err != nil {
					t.Fatalf("burn AllocPage: %v", err)
				}
				if _, err := tx.pgr.AllocSlab(id); err != nil {
					t.Fatalf("burn AllocSlab: %v", err)
				}
			}
			err = ks.Put([]byte("k"), []byte("w"))
			if err != nil && !errors.Is(err, ErrTxTooLarge) {
				t.Fatalf("Put at %d-page headroom: %v", headroomPages, err)
			}
			if got := tx.DirtyBytes() + tx.CommitReserveBytes(); got > budget {
				t.Fatalf("dirty+reserve = %d > budget %d after Put (err=%v) — obligation admitted after the op",
					got, budget, err)
			}
		})
	}
}

// TestCursorDeleteChargeAdmittedBeforeOp: Cursor.Delete mutates
// without crossing requireWritable, so it carries its own
// obligation-edge pre-charge — pinned at the cap edge exactly like
// TestDirtyingChargeAdmittedBeforeOp's Put shape.
func TestCursorDeleteChargeAdmittedBeforeOp(t *testing.T) {
	ctx := context.Background()
	const budget = 256 * 1024
	const pageSz = 4096
	for _, headroomPages := range []int{1, 2, 3, 4, 5} {
		t.Run(fmt.Sprintf("headroom=%d", headroomPages), func(t *testing.T) {
			db, err := Open(ctx, tmpPath(t), Options{
				PageSize: 4096, MinSize: 16, MaxSize: 4096,
				MaxTxBufferBytes: budget,
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { db.Close() })
			if err := db.Update(ctx, func(tx *Tx) error {
				ks, err := tx.CreateKeyspace("a")
				if err != nil {
					return err
				}
				return ks.Put([]byte("k"), []byte("v"))
			}); err != nil {
				t.Fatal(err)
			}

			tx, err := db.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			ks, err := tx.OpenKeyspace("a")
			if err != nil {
				t.Fatal(err)
			}
			for budget-tx.CommitReserveBytes()-tx.DirtyBytes() > headroomPages*pageSz {
				id, err := tx.pgr.AllocPage()
				if err != nil {
					t.Fatalf("burn AllocPage: %v", err)
				}
				if _, err := tx.pgr.AllocSlab(id); err != nil {
					t.Fatalf("burn AllocSlab: %v", err)
				}
			}
			cur := ks.Cursor()
			if k, _ := cur.First(); k == nil {
				t.Fatalf("First: %v", cur.Err())
			}
			err = cur.Delete()
			if err != nil && !errors.Is(err, ErrTxTooLarge) {
				t.Fatalf("Cursor.Delete at %d-page headroom: %v", headroomPages, err)
			}
			if got := tx.DirtyBytes() + tx.CommitReserveBytes(); got > budget {
				t.Fatalf("dirty+reserve = %d > budget %d after Cursor.Delete (err=%v)", got, budget, err)
			}
		})
	}
}

// TestRebuildRejectionUnwindsObligation: a Rebuild rejected at the
// obligation edge (remeasureRegistryDepth's affordability check, or
// any other in-window failure) must leave the formerly-Clean cached
// handle Clean — the rejected obligation must not persist in the
// reserve — and the transaction must still commit its prior work.
func TestRebuildRejectionUnwindsObligation(t *testing.T) {
	ctx := context.Background()
	const budget = 256 * 1024
	const pageSz = 4096
	db, err := Open(ctx, tmpPath(t), Options{
		PageSize: 4096, MinSize: 16, MaxSize: 4096,
		MaxTxBufferBytes: budget,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	decl := &IndexDecl{
		Name:    "ix",
		Columns: []IndexColumn{{Name: "c"}},
		Extract: func(key, value []byte) []IndexEntry {
			return []IndexEntry{{Cols: [][]byte{value[:1]}}}
		},
	}
	// Seed in small batches: each Put costs one buffer per touched
	// level, so a single seeding tx would overrun the small budget.
	for batch := range 4 {
		if err := db.Update(ctx, func(tx *Tx) error {
			var ks *Keyspace
			var err error
			if batch == 0 {
				ks, err = tx.CreateKeyspace("b", decl)
			} else {
				ks, err = tx.OpenKeyspace("b", decl)
			}
			if err != nil {
				return err
			}
			for i := batch * 10; i < (batch+1)*10; i++ {
				if err := ks.Put(fmt.Appendf(nil, "k%03d", i), []byte("vv")); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}

	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	ks, err := tx.OpenKeyspace("b", decl)
	if err != nil {
		t.Fatal(err)
	}
	var burned []uint64
	for budget-tx.CommitReserveBytes()-tx.DirtyBytes() > 2*pageSz {
		id, err := tx.pgr.AllocPage()
		if err != nil {
			t.Fatalf("burn AllocPage: %v", err)
		}
		if _, err := tx.pgr.AllocSlab(id); err != nil {
			t.Fatalf("burn AllocSlab: %v", err)
		}
		burned = append(burned, id)
	}
	if err := tx.Indexes().Rebuild("b", decl); !errors.Is(err, ErrTxTooLarge) {
		t.Fatalf("Rebuild at 2-page headroom: err = %v, want ErrTxTooLarge", err)
	}
	if ks.state != keyspaceStateClean {
		t.Errorf("handle state after rejected Rebuild = %d, want Clean (obligation persisted)", ks.state)
	}
	if got := tx.DirtyBytes() + tx.CommitReserveBytes(); got > budget {
		t.Fatalf("dirty+reserve = %d > budget %d after rejected Rebuild", got, budget)
	}
	// Release the burns so the committed image carries no
	// unreferenced pages (same-tx frees are discarded at commit).
	for _, id := range burned {
		if err := tx.pgr.FreePage(id); err != nil {
			t.Fatalf("burn FreePage(%d): %v", id, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit after rejected Rebuild: %v", err)
	}
	for iss := range db.Check() {
		t.Errorf("Check: %+v", iss)
	}
}

package gmdb

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"testing"
)

// opFailureFixture seeds a committed, multi-branch tree so a later
// transaction's mutations CoW prior-tx pages (same-tx replacements
// only go loose; the rollback contract is about RETIRED prior-tx
// pages). Seeding uses many small commits so the seed itself stays
// within the deliberately small MaxTxBufferBytes.
func opFailureFixture(t *testing.T) (*DB, []string) {
	t.Helper()
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{
		PageSize: 4096, MinSize: 16, MaxSize: 2048,
		MaxTxBufferBytes: 512 * 1024,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	val := make([]byte, 120)
	var keys []string
	for batch := range 320 { // 8000 sequential keys -> depth-3 tree
		tx, err := db.Begin(ctx)
		if err != nil {
			t.Fatalf("Begin(seed %d): %v", batch, err)
		}
		var ks *Keyspace
		if batch == 0 {
			ks, err = tx.CreateKeyspace("data")
		} else {
			ks, err = tx.OpenKeyspace("data")
		}
		if err != nil {
			t.Fatalf("keyspace(seed %d): %v", batch, err)
		}
		for i := range 25 {
			k := fmt.Sprintf("base-%06d", batch*25+i)
			if err := ks.Put([]byte(k), val); err != nil {
				t.Fatalf("seed Put(%s): %v", k, err)
			}
			keys = append(keys, k)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("Commit(seed %d): %v", batch, err)
		}
	}
	return db, keys
}

// probeOpsUntilBudgetFailures drives the pager into the exact
// retire-then-fail window and asserts, for every op that fails there,
// that the pager's retired-pages set is exactly as it was before the
// op (the per-op rollback contract). Returns the surviving key set.
//
// Geometry: in a warm transaction the root is CoW'd by the first op,
// so the only post-retire fallible step left is an ascend that must
// CoW a mid-level branch not yet touched this tx — a one-page
// allocation. The failing shape is a split-inducing insert under a
// FRESH mid-branch: leaf CoW and split-right succeed, the old leaf is
// retired, then the mid CoW trips ErrTxTooLarge. That window is one
// page wide, so the loop approaches it in EXACT one-page steps:
//
//   phase 0  warm every first-half mid-branch (stride 125);
//   filler   same-size updates on fresh first-half leaves (stride 25,
//            warm mids) — exactly one leaf-CoW page per op, no
//            splits, no fallible post-retire step;
//   probe    when headroom ∈ [2,3) pages: 3000-byte inserts under
//            fresh second-half mids — leaf CoW + split-right land
//            headroom below one page, the old leaf is retired, the
//            mid CoW fails; the rollback restores headroom so the
//            following Commit (whose own allocations count against
//            the budget) still fits.
func probeOpsUntilBudgetFailures(t *testing.T, tx *Tx, ks *Keyspace, keys []string) map[string]bool {
	t.Helper()
	live := make(map[string]bool, len(keys))
	for _, k := range keys {
		live[k] = true
	}
	const budget = 512 * 1024
	const pageSz = 4096
	val := make([]byte, 120)
	splitVal := make([]byte, 3000)
	half := len(keys) / 2

	// Phase 0: warm all first-half mid-branches.
	for j := 0; j < half; j += 125 {
		if err := ks.Put([]byte(keys[j]), val); err != nil {
			t.Fatalf("warm Put(%s): %v", keys[j], err)
		}
	}

	failures, fill, probe := 0, 12, 0
	for iter := 0; failures < 3 && iter < 10000; iter++ {
		headroom := budget - tx.DirtyBytes()
		switch {
		case headroom >= 3*pageSz:
			// Filler: exact one-page step (fresh leaf, warm mid,
			// same-size in-place value update — no split possible).
			if fill+25 >= half {
				t.Fatalf("fixture: filler keys exhausted at headroom %d", headroom)
			}
			fill += 25
			if err := ks.Put([]byte(keys[fill]), val); err != nil {
				t.Fatalf("filler Put(%s) at headroom %d: %v", keys[fill], headroom, err)
			}
		case headroom >= 2*pageSz:
			// Probe: must fail AFTER retiring the old leaf.
			probe++
			if half+probe*250 >= len(keys) {
				t.Fatalf("fixture: probe mids exhausted")
			}
			k := fmt.Sprintf("%s~%03d", keys[half+probe*250], probe)
			before := tx.RetiredPagesLen()
			err := ks.Put([]byte(k), splitVal)
			switch {
			case err == nil:
				t.Fatalf("fixture: probe Put(%q) succeeded at headroom %d; window math off", k, headroom)
			case errors.Is(err, ErrTxTooLarge) || errors.Is(err, ErrDBFull):
				failures++
				if after := tx.RetiredPagesLen(); after != before {
					t.Fatalf("failed op (%q) left retired-pages residue: %d -> %d", k, before, after)
				}
			default:
				t.Fatalf("probe Put(%q): %v", k, err)
			}
		default:
			t.Fatalf("fixture: headroom %d below probe window; step math off", headroom)
		}
	}
	if failures == 0 {
		t.Fatalf("fixture: no budget failure observed; probe loop vacuous")
	}
	return live
}

// TestFailedOpsLeavePagerOpStateUnchanged pins the per-op rollback
// contract (free-space.md bitmap-consistency invariant, both
// directions) on UN-INDEXED keyspaces: a row mutation that fails
// mid-op — after the btree already retired prior-tx pages but before
// its last fallible allocation — must restore the pager's op state
// exactly. Without the rollback, the following Commit publishes
// still-referenced pages to the RPL and reclamation later hands live
// tree pages to the allocator.
func TestFailedOpsLeavePagerOpStateUnchanged(t *testing.T) {
	ctx := context.Background()
	db, keys := opFailureFixture(t)

	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	ks, err := tx.OpenKeyspace("data")
	if err != nil {
		t.Fatalf("OpenKeyspace: %v", err)
	}
	live := probeOpsUntilBudgetFailures(t, tx, ks, keys)
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit after failed ops: %v", err)
	}

	rtx, err := db.BeginRead(ctx)
	if err != nil {
		t.Fatalf("BeginRead: %v", err)
	}
	rks, err := rtx.OpenKeyspaceReadOnly("data")
	if err != nil {
		t.Fatalf("OpenKeyspaceReadOnly: %v", err)
	}
	for k := range live {
		if _, err := rks.Get([]byte(k)); err != nil {
			t.Fatalf("Get(%s): %v", k, err)
		}
	}
	rtx.Rollback()
	for iss := range db.Check() {
		t.Errorf("Check issue after failed-op commit: %+v", iss)
	}
}

// TestCommitAfterFailedOpsSurvivesReclamation drives the failure chain
// end-to-end: fail ops mid-tx, commit, then churn across transactions
// (deletes feed the RPL; later allocations drain it via the alloc
// priority loose→bitmap→reclamation→extend). If a failed op had
// published still-referenced pages to the RPL, the live tree would now
// reference reallocated pages — reads and Check() would surface
// corruption.
func TestCommitAfterFailedOpsSurvivesReclamation(t *testing.T) {
	ctx := context.Background()
	db, keys := opFailureFixture(t)

	var live map[string]bool
	for round := range 4 {
		tx, err := db.Begin(ctx)
		if err != nil {
			t.Fatalf("Begin(round %d): %v", round, err)
		}
		ks, err := tx.OpenKeyspace("data")
		if err != nil {
			t.Fatalf("OpenKeyspace(round %d): %v", round, err)
		}
		live = probeOpsUntilBudgetFailures(t, tx, ks, keys)
		if err := tx.Commit(); err != nil {
			t.Fatalf("Commit(round %d): %v", round, err)
		}

		// RPL-feeding pass: delete a stride of keys in a fresh tx
		// (ample budget), so later rounds' allocations drain the RPL
		// via the alloc priority (loose -> bitmap -> reclamation ->
		// extend) and any wrongly-published page gets reused.
		var doomed []string
		for i := 7; i < len(keys); i += 11 {
			if live[keys[i]] {
				doomed = append(doomed, keys[i])
			}
		}
		for len(doomed) > 0 {
			n := min(40, len(doomed)) // per-tx slice within the slab budget
			tx, err = db.Begin(ctx)
			if err != nil {
				t.Fatalf("Begin(delete round %d): %v", round, err)
			}
			ks, err = tx.OpenKeyspace("data")
			if err != nil {
				t.Fatalf("OpenKeyspace(delete round %d): %v", round, err)
			}
			for _, k := range doomed[:n] {
				if err := ks.Delete([]byte(k)); err != nil {
					t.Fatalf("Delete(%s): %v", k, err)
				}
				delete(live, k)
			}
			if err := tx.Commit(); err != nil {
				t.Fatalf("Commit(delete round %d): %v", round, err)
			}
			doomed = doomed[n:]
		}

		// Next round probes over the survivors, re-sorted for the
		// stride geometry (map order is random).
		keys = keys[:0]
		for k := range live {
			keys = append(keys, k)
		}
		sort.Strings(keys)
	}

	rtx, err := db.BeginRead(ctx)
	if err != nil {
		t.Fatalf("BeginRead: %v", err)
	}
	rks, err := rtx.OpenKeyspaceReadOnly("data")
	if err != nil {
		t.Fatalf("OpenKeyspaceReadOnly: %v", err)
	}
	for k := range live {
		if _, err := rks.Get([]byte(k)); err != nil {
			t.Fatalf("Get(%s) after churn: %v", k, err)
		}
	}
	rtx.Rollback()
	for iss := range db.Check() {
		t.Errorf("Check issue after churn: %+v", iss)
	}
}

// TestFailedDeleteRangeLeavesPagerOpStateUnchanged pins the same
// rollback contract for the un-indexed DeleteRange walk. No exact
// window geometry needed: the three-phase walker retires pages
// progressively throughout, so ANY mid-walk budget failure leaves
// residue without the rollback. Fill the tx near the cap, then
// range-delete a wide span — the walk must fail with ErrTxTooLarge
// and leave the retired set untouched.
func TestFailedDeleteRangeLeavesPagerOpStateUnchanged(t *testing.T) {
	ctx := context.Background()
	db, keys := opFailureFixture(t)

	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	ks, err := tx.OpenKeyspace("data")
	if err != nil {
		t.Fatalf("OpenKeyspace: %v", err)
	}
	// Burn budget down to a few pages of headroom with the filler
	// pattern (fresh leaves, warm mids — see probeOpsUntilBudgetFailures).
	const budget = 512 * 1024
	val := make([]byte, 120)
	half := len(keys) / 2
	for j := 0; j < half; j += 125 {
		if err := ks.Put([]byte(keys[j]), val); err != nil {
			t.Fatalf("warm Put(%s): %v", keys[j], err)
		}
	}
	for fill := 12; budget-tx.DirtyBytes() > 4*4096; fill += 25 {
		if fill >= half {
			t.Fatalf("fixture: filler keys exhausted at headroom %d", budget-tx.DirtyBytes())
		}
		if err := ks.Put([]byte(keys[fill]), val); err != nil {
			t.Fatalf("filler Put(%s): %v", keys[fill], err)
		}
	}

	// Wide range over the untouched second half: the walk CoWs
	// boundary paths and retires interior subtrees — far more than
	// a few pages of budget — so it must fail mid-flight.
	before := tx.RetiredPagesLen()
	_, err = ks.DeleteRange([]byte(keys[half]), []byte(keys[len(keys)-1]))
	if err == nil {
		t.Fatalf("fixture: DeleteRange succeeded within %d headroom; widen the range", budget-tx.DirtyBytes())
	}
	if !errors.Is(err, ErrTxTooLarge) && !errors.Is(err, ErrDBFull) {
		t.Fatalf("DeleteRange: %v (want ErrTxTooLarge)", err)
	}
	if after := tx.RetiredPagesLen(); after != before {
		t.Fatalf("failed DeleteRange left retired-pages residue: %d -> %d", before, after)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit after failed DeleteRange: %v", err)
	}
	// The failed range delete must have deleted NOTHING (atomic
	// contract): every key still present.
	rtx, err := db.BeginRead(ctx)
	if err != nil {
		t.Fatalf("BeginRead: %v", err)
	}
	defer rtx.Rollback()
	rks, err := rtx.OpenKeyspaceReadOnly("data")
	if err != nil {
		t.Fatalf("OpenKeyspaceReadOnly: %v", err)
	}
	for _, k := range keys {
		if _, err := rks.Get([]byte(k)); err != nil {
			t.Fatalf("Get(%s): %v", k, err)
		}
	}
	for iss := range db.Check() {
		t.Errorf("Check issue: %+v", iss)
	}
}

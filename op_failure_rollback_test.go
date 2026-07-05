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
// retired, then the mid CoW trips ErrTxTooLarge with headroom exactly
// two pages at the probe's start.
//
// Every btree mutation on this depth-3 tree costs one fresh buffer
// per touched level (three pages for a leaf update — loose-page reuse
// recycles page IDs, not buffers), so tree ops cannot approach the
// two-page window in one-page steps. The loop therefore fills with
// tree ops to within a few pages, then lands on the window exactly
// with white-box one-page burns (AllocPage+AllocSlab), freed again
// before return so the commit publishes no unreferenced pages.
//
// Headroom is measured against the effective admission limit
// budget − CommitReserveBytes − DirtyBytes: the pager keeps the RPL
// segment cost reserved during the ops phase so Commit always fits.
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
	headroomPages := func() int {
		return (budget - tx.CommitReserveBytes() - tx.DirtyBytes()) / pageSz
	}

	// Phase 0: warm all first-half mid-branches.
	for j := 0; j < half; j += 125 {
		if err := ks.Put([]byte(keys[j]), val); err != nil {
			t.Fatalf("warm Put(%s): %v", keys[j], err)
		}
	}

	// Phase 1: fillers (same-size updates on fresh first-half leaves)
	// until within a few pages of the window.
	fill := 12
	for headroomPages() > 5 {
		if fill+25 >= half {
			t.Fatalf("fixture: filler keys exhausted at headroom %d pages", headroomPages())
		}
		fill += 25
		if err := ks.Put([]byte(keys[fill]), val); err != nil {
			t.Fatalf("filler Put(%s) at headroom %d pages: %v", keys[fill], headroomPages(), err)
		}
	}

	// Phase 2: exact one-page burns onto the two-page window.
	var burned []uint64
	for headroomPages() > 2 {
		id, err := tx.pgr.AllocPage()
		if err != nil {
			t.Fatalf("burn AllocPage: %v", err)
		}
		if _, err := tx.pgr.AllocSlab(id); err != nil {
			t.Fatalf("burn AllocSlab: %v", err)
		}
		burned = append(burned, id)
	}
	if hp := headroomPages(); hp != 2 {
		t.Fatalf("fixture: landed at headroom %d pages, want 2", hp)
	}

	// Phase 3: probes — each must fail AFTER retiring the old leaf,
	// and each failure must leave the retired set exactly as it was.
	failures := 0
	for probe := 1; failures < 3; probe++ {
		if half+probe*250 >= len(keys) {
			t.Fatalf("fixture: probe mids exhausted")
		}
		k := fmt.Sprintf("%s~%03d", keys[half+probe*250], probe)
		before := tx.RetiredPagesLen()
		err := ks.Put([]byte(k), splitVal)
		switch {
		case err == nil:
			t.Fatalf("fixture: probe Put(%q) succeeded at headroom 2 pages; window math off", k)
		case errors.Is(err, ErrTxTooLarge) || errors.Is(err, ErrDBFull):
			failures++
			if after := tx.RetiredPagesLen(); after != before {
				t.Fatalf("failed op (%q) left retired-pages residue: %d -> %d", k, before, after)
			}
		default:
			t.Fatalf("probe Put(%q): %v", k, err)
		}
	}

	// Release the burns: same-tx frees go loose and are discarded at
	// commit, so the published state carries no unreferenced pages.
	for _, id := range burned {
		if err := tx.pgr.FreePage(id); err != nil {
			t.Fatalf("burn FreePage(%d): %v", id, err)
		}
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
	// Effective headroom: admission stops at budget − CommitReserveBytes.
	for fill := 12; budget-tx.CommitReserveBytes()-tx.DirtyBytes() > 4*4096; fill += 25 {
		if fill >= half {
			t.Fatalf("fixture: filler keys exhausted at headroom %d", budget-tx.CommitReserveBytes()-tx.DirtyBytes())
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
		t.Fatalf("fixture: DeleteRange succeeded within %d headroom; widen the range", budget-tx.CommitReserveBytes()-tx.DirtyBytes())
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

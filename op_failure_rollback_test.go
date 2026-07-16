package gmdb

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"testing"

	"github.com/greatliontech/gmdb/internal/pager"
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
// allocation. The failing shape is an overflow-value insert under a
// FRESH mid-branch: the leaf CoW succeeds (consuming the last
// allocatable page), the old leaf is retired, and the mid CoW's
// AllocPage then fails ErrDBFull. The failure injector is FILE
// CAPACITY (MaxSize), the ops-phase allocation failure that remains
// now that MaxTxBufferBytes is a spill threshold rather than an
// admission ceiling: burn pages until AllocPage reports ErrDBFull,
// then free exactly ONE burn back so each probe has one page of
// allocation headroom. Each failed probe's shallow-savepoint restore
// returns that page (the probe's leaf CoW loose-popped it), so every
// probe replays the identical window — deterministic across probes.
// Burns are freed again before return so the commit publishes no
// unreferenced pages.
func probeOpsUntilBudgetFailures(t *testing.T, tx *Tx, ks *Keyspace, keys []string) map[string]bool {
	t.Helper()
	live := make(map[string]bool, len(keys))
	for _, k := range keys {
		live[k] = true
	}
	val := make([]byte, 120)
	splitVal := make([]byte, 3000)
	half := len(keys) / 2

	// Phase 0: warm all first-half mid-branches.
	for j := 0; j < half; j += 125 {
		if err := ks.Put([]byte(keys[j]), val); err != nil {
			t.Fatalf("warm Put(%s): %v", keys[j], err)
		}
	}

	// Phase 1: burn the file to capacity (AllocPage exhausts loose →
	// bitmap → reclamation → extension and reports ErrDBFull).
	var burned []uint64
	for {
		id, err := tx.pgr.AllocPage()
		if errors.Is(err, pager.ErrDBFull) {
			break
		}
		if err != nil {
			t.Fatalf("burn AllocPage: %v", err)
		}
		if _, err := tx.pgr.AllocSlab(id); err != nil {
			t.Fatalf("burn AllocSlab: %v", err)
		}
		burned = append(burned, id)
	}
	if len(burned) == 0 {
		t.Fatal("fixture: no pages burned before ErrDBFull")
	}

	// Phase 2: hand back exactly one page of allocation headroom.
	last := burned[len(burned)-1]
	burned = burned[:len(burned)-1]
	if err := tx.pgr.FreePage(last); err != nil {
		t.Fatalf("headroom FreePage(%d): %v", last, err)
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
			t.Fatalf("fixture: probe Put(%q) succeeded with one page of capacity headroom; window math off", k)
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
	// Capacity injector (see probeOpsUntilBudgetFailures): warm the
	// first-half mids, burn the file to ErrDBFull, then hand back a
	// few pages — far fewer than the wide walk's boundary CoWs need —
	// so the walk fails mid-flight after retiring pages.
	val := make([]byte, 120)
	half := len(keys) / 2
	for j := 0; j < half; j += 125 {
		if err := ks.Put([]byte(keys[j]), val); err != nil {
			t.Fatalf("warm Put(%s): %v", keys[j], err)
		}
	}
	var burned []uint64
	for {
		id, aerr := tx.pgr.AllocPage()
		if errors.Is(aerr, pager.ErrDBFull) {
			break
		}
		if aerr != nil {
			t.Fatalf("burn AllocPage: %v", aerr)
		}
		if _, serr := tx.pgr.AllocSlab(id); serr != nil {
			t.Fatalf("burn AllocSlab: %v", serr)
		}
		burned = append(burned, id)
	}
	if len(burned) == 0 {
		t.Fatal("fixture: no pages burned before ErrDBFull")
	}
	last := burned[len(burned)-1]
	burned = burned[:len(burned)-1]
	if err := tx.pgr.FreePage(last); err != nil {
		t.Fatalf("headroom FreePage(%d): %v", last, err)
	}

	// Wide range over the untouched second half: the walk CoWs
	// boundary paths and retires interior subtrees — more capacity
	// than the single free page (its own loose-page recycling covers
	// only superseded same-tx CoWs) — so it must fail mid-flight.
	before := tx.RetiredPagesLen()
	_, err = ks.DeleteRange([]byte(keys[half]), []byte(keys[len(keys)-1]))
	if err == nil {
		t.Fatal("fixture: DeleteRange succeeded with one page of capacity headroom; window math off")
	}
	if !errors.Is(err, ErrTxTooLarge) && !errors.Is(err, ErrDBFull) {
		t.Fatalf("DeleteRange: %v (want ErrDBFull)", err)
	}
	if after := tx.RetiredPagesLen(); after != before {
		t.Fatalf("failed DeleteRange left retired-pages residue: %d -> %d", before, after)
	}
	for _, id := range burned {
		if err := tx.pgr.FreePage(id); err != nil {
			t.Fatalf("burn FreePage(%d): %v", id, err)
		}
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

package gmdb

import (
	"bytes"
	"context"
	"fmt"
	"testing"

	"github.com/greatliontech/gmdb/internal/btree"
	"github.com/greatliontech/gmdb/internal/page"
)

// Over-threshold set values (limits.md §Maximum Value Size (Set
// Keyspaces)): a set value shares the KEY bound — over-`T` members
// become overflow-key cells in the nested tree; an over-`T` value may
// legally reside in a subpage while the set stays below the promotion
// threshold; a first value past the promotion budget goes straight to
// a single-member nested tree.

// setRootCell fetches the outer-tree cell for key — white-box shape
// inspection (subpage vs nested) for the storage-strategy pins.
func setRootCell(t *testing.T, tx *Tx, sks *SetKeyspace, key []byte) page.LeafEntry {
	t.Helper()
	e, found, err := btree.GetEntry(tx.pgr, sks.builderCfg(), sks.desc.Root, key)
	if err != nil || !found {
		t.Fatalf("GetEntry(%q): found=%v err=%v", key, found, err)
	}
	return e
}

func checkClean(t *testing.T, db *DB) {
	t.Helper()
	for _, is := range collectIssues(db.Check()) {
		t.Errorf("Check issue: %+v", is)
	}
}

// TestSetKeyspaceOverThresholdValues drives every storage transition
// with over-`T` members: straight-to-nested genesis, the subpage
// window, promotion with mixed sizes, deletion, and the demotion
// sliver — verifying membership across commit and a clean Check
// (leaked member extents would surface as BitmapLeak).
func TestSetKeyspaceOverThresholdValues(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 1 << 16,
		Maintenance: MaintenanceOptions{Disable: true}})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	tx, _ := db.Begin(ctx)
	sks, err := tx.CreateSetKeyspace("s", nil)
	if err != nil {
		t.Fatalf("CreateSetKeyspace: %v", err)
	}
	cfg := sks.builderCfg()
	tSz := cfg.InlineThreshold()
	budget := page.SubpagePromotionThreshold(cfg)

	// (1) Genesis past the promotion budget: straight to a
	// single-member nested tree with an overflow-key member.
	big := bytes.Repeat([]byte{'B'}, budget+500)
	if _, err := sks.Put([]byte("nested-genesis"), big); err != nil {
		t.Fatalf("Put big genesis: %v", err)
	}
	if e := setRootCell(t, tx, sks, []byte("nested-genesis")); !e.IsNestedTree() {
		t.Errorf("over-budget first value: cell flags 0x%x, want nested-tree", e.Flags)
	}
	if has, err := sks.HasValue([]byte("nested-genesis"), big); err != nil || !has {
		t.Fatalf("HasValue(big): has=%v err=%v", has, err)
	}

	// (2) The (T, budget] subpage window: an over-T value that fits
	// the subpage stays a subpage.
	window := bytes.Repeat([]byte{'W'}, tSz+2)
	if got := len(window) + 2 + page.SubpageHeaderSize; got > subpageBudgetForKey(cfg, []byte("window")) {
		t.Fatalf("fixture: window subpage %d exceeds the per-key budget %d", got, subpageBudgetForKey(cfg, []byte("window")))
	}
	if _, err := sks.Put([]byte("window"), window); err != nil {
		t.Fatalf("Put window: %v", err)
	}
	if e := setRootCell(t, tx, sks, []byte("window")); !e.IsSubpage() {
		t.Errorf("window value: cell flags 0x%x, want subpage", e.Flags)
	}
	if has, err := sks.HasValue([]byte("window"), window); err != nil || !has {
		t.Fatalf("HasValue(window): has=%v err=%v", has, err)
	}

	// (3) Promotion with over-T members: a second window-sized value
	// pushes the subpage past the budget — the nested tree holds BOTH
	// as overflow-key members.
	window2 := bytes.Repeat([]byte{'X'}, tSz+20)
	if _, err := sks.Put([]byte("window"), window2); err != nil {
		t.Fatalf("Put window2 (promotion): %v", err)
	}
	if e := setRootCell(t, tx, sks, []byte("window")); !e.IsNestedTree() {
		t.Errorf("after promotion: cell flags 0x%x, want nested-tree", e.Flags)
	}
	for _, v := range [][]byte{window, window2} {
		if has, err := sks.HasValue([]byte("window"), v); err != nil || !has {
			t.Fatalf("HasValue(post-promotion %c): has=%v err=%v", v[0], has, err)
		}
	}
	if n, err := sks.CountValues([]byte("window")); err != nil || n != 2 {
		t.Fatalf("CountValues = %d, err=%v, want 2", n, err)
	}

	// (4) Mixed small + over-T members iterate in order via the cursor.
	members := [][]byte{
		[]byte("aaa"),
		bytes.Repeat([]byte{'m'}, tSz+5),
		bytes.Repeat([]byte{'n'}, tSz+700),
		[]byte("zzz"),
	}
	for _, m := range members {
		if _, err := sks.Put([]byte("mixed"), m); err != nil {
			t.Fatalf("Put mixed %d bytes: %v", len(m), err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// Cross-commit readback of every shape.
	rtx, _ := db.BeginRead(ctx)
	rsks, err := rtx.OpenSetKeyspaceReadOnly("s")
	if err != nil {
		t.Fatalf("OpenSetKeyspaceReadOnly: %v", err)
	}
	for _, probe := range []struct {
		key string
		val []byte
	}{
		{"nested-genesis", big},
		{"window", window},
		{"window", window2},
		{"mixed", members[1]},
		{"mixed", members[2]},
	} {
		if has, err := rsks.HasValue([]byte(probe.key), probe.val); err != nil || !has {
			t.Fatalf("committed HasValue(%s, %d bytes): has=%v err=%v", probe.key, len(probe.val), has, err)
		}
	}
	c := rsks.Cursor()
	got := make([][]byte, 0, len(members))
	k, v := c.Seek([]byte("mixed"))
	for v != nil && string(k) == "mixed" {
		got = append(got, bytes.Clone(v))
		v = c.NextValue()
	}
	if err := c.Err(); err != nil {
		t.Fatalf("cursor: %v", err)
	}
	if len(got) != len(members) {
		t.Fatalf("cursor over mixed: %d values, want %d", len(got), len(members))
	}
	for i := range members {
		if !bytes.Equal(got[i], members[i]) {
			t.Errorf("cursor value %d: %d bytes (first %q), want %d bytes", i, len(got[i]), got[i][:1], len(members[i]))
		}
	}
	rtx.Rollback()
	checkClean(t, db)

	// (5) DeleteValue of over-T members retires their extents; the
	// demotion sliver — a single leftover over-T member whose subpage
	// fits the budget — demotes back to a subpage with the full bytes.
	tx2, _ := db.Begin(ctx)
	sks2, _ := tx2.OpenSetKeyspace("s")
	if err := sks2.DeleteValue([]byte("window"), window2); err != nil {
		t.Fatalf("DeleteValue(window2): %v", err)
	}
	if e := setRootCell(t, tx2, sks2, []byte("window")); !e.IsSubpage() {
		t.Errorf("after delete-to-one over-T member: cell flags 0x%x, want demoted subpage", e.Flags)
	}
	if has, err := sks2.HasValue([]byte("window"), window); err != nil || !has {
		t.Fatalf("HasValue after demotion: has=%v err=%v", has, err)
	}
	if err := sks2.Delete([]byte("nested-genesis")); err != nil {
		t.Fatalf("Delete(nested-genesis): %v", err)
	}
	if err := tx2.Commit(); err != nil {
		t.Fatalf("Commit 2: %v", err)
	}
	// Deletion + demotion must leave no orphan extents or runs.
	checkClean(t, db)
}

// TestSetKeyspaceBulkOverThresholdValues: the bulk path stores the
// same shapes as Put — over-budget first values, window residents,
// over-T nested members — and the loaded keyspace round-trips and
// Checks clean.
func TestSetKeyspaceBulkOverThresholdValues(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 1 << 16,
		Maintenance: MaintenanceOptions{Disable: true}})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	tx, _ := db.Begin(ctx)
	sks, err := tx.CreateSetKeyspace("s", nil)
	if err != nil {
		t.Fatalf("CreateSetKeyspace: %v", err)
	}
	cfg := sks.builderCfg()
	tSz := cfg.InlineThreshold()
	budget := page.SubpagePromotionThreshold(cfg)

	big := bytes.Repeat([]byte{'B'}, budget+500)
	window := bytes.Repeat([]byte{'W'}, tSz+2)
	small := []byte("aa")
	overT1 := bytes.Repeat([]byte{'p'}, tSz+3)
	overT2 := bytes.Repeat([]byte{'q'}, tSz+900)
	jumbo := bytes.Repeat([]byte{'r'}, 70000)     // past the u16 KeyLen range — extent-only territory
	longKey := bytes.Repeat([]byte{'K'}, tSz+123) // over-T OUTER set key

	// A first value in the (per-key budget, global threshold] band:
	// over the key's splittability budget though under the 50%
	// threshold — must go nested (a subpage cell here could strand an
	// un-splittable leaf).
	edge := bytes.Repeat([]byte{'E'}, budget-page.SubpageHeaderSize-2)
	// Ascending outer-key order ('K' < 'k'): the long key first.
	rows := [][2][]byte{
		{longKey, window},
		{[]byte("k1-big-first"), big},
		{[]byte("k2-window"), window},
		{[]byte("k3-mixed"), small},
		{[]byte("k3-mixed"), overT1},
		{[]byte("k3-mixed"), overT2},
		{[]byte("k3-mixed"), jumbo},
		{[]byte("k4-edge"), edge},
	}
	n, err := sks.BulkLoad(func(yield func([]byte, []byte) bool) {
		for _, r := range rows {
			if !yield(r[0], r[1]) {
				return
			}
		}
	})
	if err != nil {
		t.Fatalf("BulkLoad: %v", err)
	}
	if n != uint64(len(rows)) {
		t.Fatalf("BulkLoad count = %d, want %d", n, len(rows))
	}
	// Shape parity with the Put path.
	if e := setRootCell(t, tx, sks, []byte("k1-big-first")); !e.IsNestedTree() {
		t.Errorf("bulk over-budget first value: flags 0x%x, want nested-tree", e.Flags)
	}
	if e := setRootCell(t, tx, sks, []byte("k2-window")); !e.IsSubpage() {
		t.Errorf("bulk window value: flags 0x%x, want subpage", e.Flags)
	}
	if e := setRootCell(t, tx, sks, []byte("k4-edge")); !e.IsNestedTree() {
		t.Errorf("bulk budget-band first value: flags 0x%x, want nested-tree (per-key splittability cap)", e.Flags)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	rtx, _ := db.BeginRead(ctx)
	rsks, _ := rtx.OpenSetKeyspaceReadOnly("s")
	for _, r := range rows {
		if has, err := rsks.HasValue(r[0], r[1]); err != nil || !has {
			t.Fatalf("HasValue(%d-byte key, %d-byte value): has=%v err=%v", len(r[0]), len(r[1]), has, err)
		}
	}
	rtx.Rollback()
	checkClean(t, db)
}

// TestCopyToCompactLongSetMembers: the compacting rebuild
// re-accumulates sets with over-T members and over-T outer keys
// through the bulk path — the copy round-trips and Checks clean.
func TestCopyToCompactLongSetMembers(t *testing.T) {
	ctx := context.Background()
	src := tmpPath(t)
	db, err := Open(ctx, src, Options{PageSize: 4096, MinSize: 16, MaxSize: 1 << 16,
		Maintenance: MaintenanceOptions{Disable: true}})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	tx, _ := db.Begin(ctx)
	sks, _ := tx.CreateSetKeyspace("s", nil)
	cfg := sks.builderCfg()
	tSz := cfg.InlineThreshold()
	budget := page.SubpagePromotionThreshold(cfg)
	big := bytes.Repeat([]byte{'B'}, budget+500)
	overT := bytes.Repeat([]byte{'m'}, tSz+40)
	longKey := bytes.Repeat([]byte{'K'}, tSz+321)
	for _, kv := range [][2][]byte{
		{[]byte("k1"), big},
		{[]byte("k1"), overT},
		{[]byte("k2"), overT},
		{longKey, big},
	} {
		if _, err := sks.Put(kv[0], kv[1]); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	// A long-key PLAIN keyspace rides along so the KV rebuild is
	// covered in the same image.
	ks, _ := tx.CreateKeyspace("kv")
	if err := ks.Put(longKey, bytes.Repeat([]byte{'v'}, 9000)); err != nil {
		t.Fatalf("Put kv: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	dst := tmpPath(t)
	if err := db.CopyTo(dst, true); err != nil {
		t.Fatalf("CopyTo(compact): %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	db2, err := Open(ctx, dst, Options{PageSize: 4096, MinSize: 16, MaxSize: 1 << 16,
		Maintenance: MaintenanceOptions{Disable: true}})
	if err != nil {
		t.Fatalf("re-Open copy: %v", err)
	}
	defer db2.Close()
	rtx, _ := db2.BeginRead(ctx)
	rsks, err := rtx.OpenSetKeyspaceReadOnly("s")
	if err != nil {
		t.Fatalf("open set in copy: %v", err)
	}
	for _, kv := range [][2][]byte{
		{[]byte("k1"), big},
		{[]byte("k1"), overT},
		{[]byte("k2"), overT},
		{longKey, big},
	} {
		if has, err := rsks.HasValue(kv[0], kv[1]); err != nil || !has {
			t.Fatalf("copy HasValue(%d-byte key, %d-byte value): has=%v err=%v", len(kv[0]), len(kv[1]), has, err)
		}
	}
	rks, _ := rtx.OpenKeyspaceReadOnly("kv")
	if v, err := rks.Get(longKey); err != nil || len(v) != 9000 {
		t.Fatalf("copy Get(longKey): len=%d err=%v", len(v), err)
	}
	rtx.Rollback()
	checkClean(t, db2)
}

// TestSetLongMembersSurviveCompaction: incremental-compaction
// relocation moves nested trees with overflow-key members (and their
// extents) without losing membership.
func TestSetLongMembersSurviveCompaction(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 1 << 20,
		Maintenance: MaintenanceOptions{Disable: true}})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	// Fillers FIRST so their pages take low ids; the set data lands
	// above them; deleting the fillers then opens a free band BELOW
	// the data for the relocation to consolidate into.
	tx0, _ := db.Begin(ctx)
	ks, _ := tx0.CreateKeyspace("filler")
	for i := range 400 {
		if err := ks.Put(fmt.Appendf(nil, "f%05d", i), bytes.Repeat([]byte{'f'}, 64)); err != nil {
			t.Fatalf("Put filler: %v", err)
		}
	}
	if err := tx0.Commit(); err != nil {
		t.Fatalf("Commit fillers: %v", err)
	}

	tx, _ := db.Begin(ctx)
	sks, _ := tx.CreateSetKeyspace("s", nil)
	cfg := sks.builderCfg()
	tSz := cfg.InlineThreshold()
	var members [][]byte
	for i := range 6 {
		m := append(bytes.Repeat([]byte{byte('a' + i)}, tSz+50), fmt.Sprintf("%03d", i)...)
		members = append(members, m)
		if _, err := sks.Put([]byte("k"), m); err != nil {
			t.Fatalf("Put member %d: %v", i, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	txd, _ := db.Begin(ctx)
	if err := txd.DeleteKeyspace("filler"); err != nil {
		t.Fatalf("DeleteKeyspace: %v", err)
	}
	if err := txd.Commit(); err != nil {
		t.Fatalf("Commit delete: %v", err)
	}
	// Nudge commits drain the RPL so the freed filler pages become
	// bitmap-free capacity the relocation can allocate from.
	for i := range 3 {
		txn, _ := db.Begin(ctx)
		ksn, _ := txn.OpenSetKeyspace("s")
		if _, err := ksn.Put([]byte("nudge"), fmt.Appendf(nil, "n%02d", i)); err != nil {
			t.Fatalf("nudge Put: %v", err)
		}
		if err := txn.Commit(); err != nil {
			t.Fatalf("nudge Commit: %v", err)
		}
	}

	// Relocate the forest into the freed capacity (the shared harness
	// halves the batch on below-floor exhaustion, mirroring the
	// production driver).
	if moved := runCompactForest(t, db, 0, 64); moved == 0 {
		t.Fatal("relocation moved nothing — fixture failed to exercise nested-member extents")
	}

	rtx, _ := db.BeginRead(ctx)
	rsks, _ := rtx.OpenSetKeyspaceReadOnly("s")
	for i, m := range members {
		if has, err := rsks.HasValue([]byte("k"), m); err != nil || !has {
			t.Fatalf("post-relocation HasValue(member %d): has=%v err=%v", i, has, err)
		}
	}
	rtx.Rollback()
	checkClean(t, db)
}

// TestIndexLongCompositeKeysMaintenanceAndRebuild: long composite
// index keys (limits.md §Maximum Key Size — extractor-produced keys
// share the ordinary key contract) through PER-ROW maintenance (Put /
// Delete on an open indexed keyspace) and through Rebuild (the
// extsort + bulk-build path), with lookups resolving both ways.
func TestIndexLongCompositeKeysMaintenanceAndRebuild(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 1 << 16,
		Maintenance: MaintenanceOptions{Disable: true}})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	col := func(pk []byte) []byte {
		return append(bytes.Repeat([]byte{'C'}, 3000), pk...)
	}
	extract := func(k, _ []byte) []IndexEntry {
		return []IndexEntry{{Cols: [][]byte{col(k)}}}
	}
	decl := &IndexDecl{Name: "byc", Columns: []IndexColumn{{Name: "c"}}, Extract: extract}

	// Per-row maintenance: Put with the index open.
	tx, _ := db.Begin(ctx)
	ks, err := tx.CreateKeyspace("k", decl)
	if err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	for i := range 8 {
		if err := ks.Put(fmt.Appendf(nil, "pk%02d", i), []byte("v")); err != nil {
			t.Fatalf("Put(%d): %v", i, err)
		}
	}
	if err := ks.Delete([]byte("pk03")); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	idx, err := ks.Index("byc")
	if err != nil {
		t.Fatalf("Index: %v", err)
	}
	lookup := func(pk []byte) int {
		n := 0
		for range idx.LookupKeys([][]byte{col(pk)}) {
			n++
		}
		if err := idx.Err(); err != nil {
			t.Fatalf("lookup err: %v", err)
		}
		return n
	}
	if n := lookup([]byte("pk01")); n != 1 {
		t.Fatalf("lookup(pk01) = %d, want 1", n)
	}
	if n := lookup([]byte("pk03")); n != 0 {
		t.Fatalf("lookup(deleted pk03) = %d, want 0 (index row not removed)", n)
	}

	// Rebuild (extsort + bulk build) reproduces the same long-key
	// index tree.
	if err := tx.Indexes().Rebuild("k", decl); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	idx2, err := ks.Index("byc")
	if err != nil {
		t.Fatalf("Index post-rebuild: %v", err)
	}
	n := 0
	for range idx2.LookupKeys([][]byte{col([]byte("pk05"))}) {
		n++
	}
	if err := idx2.Err(); err != nil {
		t.Fatalf("post-rebuild lookup err: %v", err)
	}
	if n != 1 {
		t.Fatalf("post-rebuild lookup = %d, want 1", n)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	checkClean(t, db)
}

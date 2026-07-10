package gmdb

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"testing"
)

// Subpage promotion must never fail for capacity reasons on an
// in-spec set (set-keyspace.md §Subpage Promotion Threshold): a leaf
// cell carries per-entry overhead a subpage entry does not, so a
// threshold-sized subpage of SMALL members does not fit one nested
// leaf — the promotion builds a multi-leaf tree. Previously the
// single-leaf copy hard-capped small-member sets: 8-byte fixed-size
// members (the canonical postings/ID workload) failed at Put #254
// with "nested-root leaf overflowed", and every subsequent
// new-member Put re-attempted promotion and failed forever.

// hashLikeMember spreads bytes so members share no useful prefix —
// the worst case for leaf density (no compression rescue).
func hashLikeMember(i int) []byte {
	v := make([]byte, 8)
	binary.BigEndian.PutUint64(v, uint64(i)*0x9E3779B97F4A7C15)
	return v
}

func TestSetKeyspacePromotionSmallFixedMembersScalesPastOneLeaf(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 512})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	tx, _ := db.Begin(ctx)
	defer tx.Rollback()

	sks, err := tx.CreateSetKeyspace("ids", &SetKeyspaceOptions{FixedValueSize: 8})
	if err != nil {
		t.Fatalf("CreateSetKeyspace: %v", err)
	}
	const n = 2000 // far past the old ~254 hard cap
	for i := range n {
		added, err := sks.Put([]byte("topic"), hashLikeMember(i))
		if err != nil {
			t.Fatalf("Put #%d: %v", i, err)
		}
		if !added {
			t.Fatalf("Put #%d: added=false for a fresh member", i)
		}
	}
	cnt, err := sks.CountValues([]byte("topic"))
	if err != nil || cnt != n {
		t.Fatalf("CountValues = (%d, %v), want %d", cnt, err, n)
	}
	// The representation must be a nested tree (a 16 KB member set
	// cannot be a subpage) — pins that promotion actually fired.
	if e, ok, gerr := getEntryForTestKey(sks, sks.builderCfg(), []byte("topic")); gerr != nil || !ok || !e.IsNestedTree() {
		t.Fatalf("post-promotion cell = (found=%v err=%v flags=%#x), want nested tree", ok, gerr, e.Flags)
	}
	// Every member present, and the value stream is sorted + complete.
	for i := range n {
		ok, err := sks.HasValue([]byte("topic"), hashLikeMember(i))
		if err != nil || !ok {
			t.Fatalf("HasValue #%d = (%v, %v), want present", i, ok, err)
		}
	}
	// Stream all (key, value) pairs — one key, n values, sorted.
	var prev []byte
	seen := 0
	for _, v := range sks.All() {
		if prev != nil && bytes.Compare(prev, v) >= 0 {
			t.Fatalf("values out of order at #%d", seen)
		}
		prev = bytes.Clone(v)
		seen++
	}
	if seen != n {
		t.Fatalf("streamed %d values, want %d", seen, n)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
}

// Same class with tiny variable-size members (old cap ~508).
func TestSetKeyspacePromotionSmallVariableMembersScalesPastOneLeaf(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 512})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	tx, _ := db.Begin(ctx)
	defer tx.Rollback()

	sks, err := tx.CreateSetKeyspace("tags", nil)
	if err != nil {
		t.Fatalf("CreateSetKeyspace: %v", err)
	}
	const n = 1500
	member := func(i int) []byte { return fmt.Appendf(nil, "%04x", i) }
	for i := range n {
		if _, err := sks.Put([]byte("post"), member(i)); err != nil {
			t.Fatalf("Put #%d: %v", i, err)
		}
	}
	cnt, err := sks.CountValues([]byte("post"))
	if err != nil || cnt != n {
		t.Fatalf("CountValues = (%d, %v), want %d", cnt, err, n)
	}
	// Deleting back below the demotion boundary still works (the
	// multi-leaf tree demotes through the existing single-leaf path
	// once shrunk).
	for i := 20; i < n; i++ {
		if err := sks.DeleteValue([]byte("post"), member(i)); err != nil {
			t.Fatalf("DeleteValue #%d: %v", i, err)
		}
	}
	cnt, err = sks.CountValues([]byte("post"))
	if err != nil || cnt != 20 {
		t.Fatalf("post-shrink CountValues = (%d, %v), want 20", cnt, err)
	}
	// The shrink must have DEMOTED the multi-leaf tree back to a
	// subpage — this is the new territory (multi-leaf → root collapse
	// → single leaf → demote); Count/Has answers alone would pass
	// with demotion broken.
	if e, ok, gerr := getEntryForTestKey(sks, sks.builderCfg(), []byte("post")); gerr != nil || !ok || !e.IsSubpage() {
		t.Fatalf("post-shrink cell = (found=%v err=%v flags=%#x), want subpage (demotion)", ok, gerr, e.Flags)
	}
	for i := range 20 {
		ok, err := sks.HasValue([]byte("post"), member(i))
		if err != nil || !ok {
			t.Fatalf("HasValue #%d after shrink = (%v, %v)", i, ok, err)
		}
	}
}

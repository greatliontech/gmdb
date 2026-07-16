package gmdb

import (
	"bytes"
	"context"
	"testing"
)

// In-spec per limits.md: set values share the uint32 KEY bound.
// A member past the subpage uint16 DataSize cap must route to a
// nested tree, not error out of the subpage encoder.

func openSetForBigMembers(t *testing.T) (*DB, *Tx, *SetKeyspace) {
	t.Helper()
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 1 << 16})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	sks, err := tx.CreateSetKeyspace("s", nil)
	if err != nil {
		t.Fatalf("CreateSetKeyspace: %v", err)
	}
	return db, tx, sks
}

// A: genesis Put of a >64KB member.
func TestSetGenesisMemberPastSubpageCapGoesNested(t *testing.T) {
	db, _, sks := openSetForBigMembers(t)
	defer db.Close()
	big := bytes.Repeat([]byte{0x42}, 70000)
	added, err := sks.Put([]byte("k"), big)
	if err != nil {
		t.Fatalf("genesis Put of in-spec 70000-byte member failed: %v", err)
	}
	if !added {
		t.Fatal("not added")
	}
	has, err := sks.HasValue([]byte("k"), big)
	if err != nil || !has {
		t.Fatalf("readback: has=%v err=%v", has, err)
	}
}

// B: second-value Put of a >64KB member into an existing subpage.
func TestSetSubpageInsertMemberPastCapPromotes(t *testing.T) {
	db, _, sks := openSetForBigMembers(t)
	defer db.Close()
	if _, err := sks.Put([]byte("k"), []byte("small")); err != nil {
		t.Fatalf("small Put: %v", err)
	}
	big := bytes.Repeat([]byte{0x43}, 70000)
	if _, err := sks.Put([]byte("k"), big); err != nil {
		t.Fatalf("subpage-insert Put of in-spec 70000-byte member failed: %v", err)
	}
}

// C: DeleteValue that triggers demotion over a >64KB surviving member.
func TestSetDemotionDeclinesOverBigMember(t *testing.T) {
	db, _, sks := openSetForBigMembers(t)
	defer db.Close()
	big := bytes.Repeat([]byte{0x44}, 70000)
	// Build a nested tree with {big, small} members via whatever path works.
	if _, err := sks.Put([]byte("k"), big); err != nil {
		t.Fatalf("genesis big Put: %v", err)
	}
	if _, err := sks.Put([]byte("k"), []byte("zz")); err != nil {
		t.Fatalf("second Put: %v", err)
	}
	if err := sks.DeleteValue([]byte("k"), []byte("zz")); err != nil {
		t.Fatalf("DeleteValue triggering demotion over a 70000-byte member failed: %v", err)
	}
	has, err := sks.HasValue([]byte("k"), big)
	if err != nil || !has {
		t.Fatalf("readback after demotion attempt: has=%v err=%v", has, err)
	}
}

// C2: demotion via a bulk-loaded nested tree with a >64KB member.
func TestSetDemotionDeclinesOverBigMemberBulk(t *testing.T) {
	db, _, sks := openSetForBigMembers(t)
	defer db.Close()
	big := bytes.Repeat([]byte{0x45}, 70000)
	rows := func(yield func([]byte, []byte) bool) {
		_ = yield([]byte("k"), big) && yield([]byte("k"), []byte("zz"))
	}
	if _, err := sks.BulkLoad(rows); err != nil {
		t.Fatalf("BulkLoad: %v", err)
	}
	if err := sks.DeleteValue([]byte("k"), []byte("zz")); err != nil {
		t.Fatalf("DeleteValue triggering demotion over a 70000-byte member failed: %v", err)
	}
	has, err := sks.HasValue([]byte("k"), big)
	if err != nil || !has {
		t.Fatalf("readback: has=%v err=%v", has, err)
	}
}

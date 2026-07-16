package gmdb

import (
	"bytes"
	"context"
	"fmt"
	"testing"
)

// TestDeleteKeyspaceRetiresLiveIndexRoots: same-tx index maintenance
// moves the pinned index data root in memory; the registry sub-tree
// is synced only at flush. DeleteKeyspace's registry-walk retirement
// must resolve each entry's root through the LIVE pinned root (the
// dropIndex liveIndexRoot guard) — walking the registry's stale root
// double-frees superseded pages and leaks every page the live index
// tree gained this tx (BitmapLeak on Check; under the spill
// machinery the stale walk can also dereference dropped loose
// buffers and fail ErrBadPageChecksum mid-walk).
func TestDeleteKeyspaceRetiresLiveIndexRoots(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 4096,
		Maintenance: MaintenanceOptions{Disable: true}})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	decl := func() *IndexDecl {
		return &IndexDecl{Name: "ix", Columns: []IndexColumn{{Name: "c"}},
			Extract: func(key, value []byte) []IndexEntry {
				return []IndexEntry{{Cols: [][]byte{bytes.Clone(value[:1])}}}
			}}
	}
	// tx1: create + seed enough rows that the index tree is multi-page.
	tx1, _ := db.Begin(ctx)
	ks1, err := tx1.CreateKeyspace("k", decl())
	if err != nil {
		t.Fatal(err)
	}
	val := make([]byte, 200)
	for i := range 200 {
		val[0] = byte('a' + i%20)
		if err := ks1.Put(fmt.Appendf(nil, "r%04d", i), val); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx1.Commit(); err != nil {
		t.Fatal(err)
	}
	// tx2: more index-moving puts through an open handle, then delete
	// the keyspace in the same tx.
	tx2, _ := db.Begin(ctx)
	ks2, err := tx2.OpenKeyspace("k", decl())
	if err != nil {
		t.Fatal(err)
	}
	for i := range 200 {
		val[0] = byte('A' + i%20)
		if err := ks2.Put(fmt.Appendf(nil, "s%04d", i), val); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx2.DeleteKeyspace("k"); err != nil {
		t.Fatal(err)
	}
	if err := tx2.Commit(); err != nil {
		t.Fatal(err)
	}
	n := 0
	for iss := range db.Check() {
		t.Errorf("Check: %+v", iss)
		n++
		if n > 5 {
			break
		}
	}
}

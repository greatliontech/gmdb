package query_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/greatliontech/gmdb"
	"github.com/greatliontech/gmdb/query"
	"github.com/greatliontech/gmdb/typed"
)

// TestQueryOverSnapshotReadTx pins gmdb/query execution over a typed
// handle opened from a snapshot read transaction: the query runs
// against the pinned snapshot without a write transaction, planning
// as a full scan (a read-only handle carries no planner index
// metadata; results identical per Inv-QB1).
func TestQueryOverSnapshotReadTx(t *testing.T) {
	ctx := context.Background()
	db, err := gmdb.Open(ctx, filepath.Join(t.TempDir(), "db.gmdb"),
		gmdb.Options{PageSize: 4096, MinSize: 16, MaxSize: 512})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	tks := typed.NewKeyspace[uint64, row]("rows", typed.Uint64Encoder{}, rowCodec{})
	idxGrp := ci("g", anyCols(colGrp), typed.ColumnIndexOpts[uint64, row]{})
	corpus := map[uint64]row{
		1: {Grp: 7, Name: "a"},
		2: {Grp: 7, Name: "b"},
		3: {Grp: 8, Name: "c"},
	}
	if err := db.Update(ctx, func(tx *gmdb.Tx) error {
		h, err := tks.Create(tx, idxGrp)
		if err != nil {
			return err
		}
		for k, v := range corpus {
			if err := h.Put(k, v); err != nil {
				return err
			}
		}
		// The write handle carries the declaration's planner metadata:
		// the same predicate plans as an index seek here, so the scan
		// asserted over the ReadTx-opened handle below is attributable
		// to OpenReadOnly carrying none — not to the corpus lacking an
		// index.
		if kind, _ := planLeafOf(query.New(h).Where(colGrp.Eq(7)).Explain()); kind != "seek" {
			t.Fatalf("write-handle plan leaf = %q, want seek", kind)
		}
		return nil
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	if err := db.View(ctx, func(rtx *gmdb.ReadTx) error {
		h, err := tks.OpenReadOnly(rtx)
		if err != nil {
			t.Fatalf("OpenReadOnly(ReadTx): %v", err)
		}
		q := query.New(h).Where(colGrp.Eq(7))
		if kind, _ := planLeafOf(q.Explain()); kind != "scan" {
			t.Fatalf("plan = %s, want a Scan leaf (read-only handle carries no planner index metadata)", q.Explain())
		}
		got := map[uint64]string{}
		for k, v := range q.All() {
			got[k] = v.Name
		}
		if err := q.Err(); err != nil {
			t.Fatalf("Err: %v", err)
		}
		if len(got) != 2 || got[1] != "a" || got[2] != "b" {
			t.Fatalf("query over snapshot = %v, want map[1:a 2:b]", got)
		}
		return nil
	}); err != nil {
		t.Fatalf("View: %v", err)
	}
}

package gmdb_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/thegrumpylion/gmdb"
	"github.com/thegrumpylion/gmdb/query"
	"github.com/thegrumpylion/gmdb/typed"
)

// Each query execution opens a fresh typed cursor; releasing it via
// Cursor.Close is what keeps the keyspace's per-mutation staleness
// walk O(live cursors) instead of O(executions) across a long
// transaction. The byte handle is the same per-tx cached instance
// the typed tier scans over, so its registration count observes the
// release end-to-end.
func TestQueryExecutionReleasesItsCursor(t *testing.T) {
	ctx := context.Background()
	db, err := gmdb.Open(ctx, filepath.Join(t.TempDir(), "db.gmdb"),
		gmdb.Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()

	tks := typed.NewKeyspace[uint64, string]("rows", typed.Uint64Encoder{}, typed.StringEncoder{})
	h, err := tks.Create(tx)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	for k, v := range map[uint64]string{1: "a", 2: "b", 3: "c"} {
		if err := h.Put(k, v); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	ks, err := tx.OpenKeyspace("rows")
	if err != nil {
		t.Fatalf("OpenKeyspace: %v", err)
	}

	base := ks.OpenCursorCountForTest()
	for i := 0; i < 5; i++ {
		q := query.New(h)
		n := 0
		for range q.All() {
			n++
		}
		if q.Err() != nil || n != 3 {
			t.Fatalf("execution %d: rows=%d Err=%v, want 3 rows nil Err", i, n, q.Err())
		}
	}
	if got := ks.OpenCursorCountForTest(); got != base {
		t.Fatalf("registered cursors after 5 executions = %d, want %d (each execution must Close its cursor)", got, base)
	}

	// A caller break mid-iteration exits through the same release
	// path (the iterator closure's defer).
	q := query.New(h)
	for range q.All() {
		break
	}
	if got := ks.OpenCursorCountForTest(); got != base {
		t.Fatalf("registered cursors after early break = %d, want %d", got, base)
	}
}

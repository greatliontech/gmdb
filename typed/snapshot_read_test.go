package typed

import (
	"context"
	"errors"
	"maps"
	"testing"

	"github.com/greatliontech/gmdb"
)

// TestKeyspaceSnapshotReadOnly pins the typed read path over a snapshot
// read transaction (*gmdb.ReadTx via the ReadOpener surface): reads
// observe the pinned snapshot (a commit after BeginRead stays
// invisible), every mutator returns gmdb.ErrReadOnly, and the handle
// dies with the ReadTx (gmdb.ErrTxClosed).
func TestKeyspaceSnapshotReadOnly(t *testing.T) {
	ctx := context.Background()
	db := openWith(t, ctx, tmpPath(t), gmdb.Options{PageSize: 4096, MinSize: 16, MaxSize: 256})
	defer db.Close()

	tks := NewKeyspace("ks", StringEncoder{}, StringEncoder{})
	if err := db.Update(ctx, func(tx *gmdb.Tx) error {
		h, err := tks.Create(tx)
		if err != nil {
			return err
		}
		if err := h.Put("a", "1"); err != nil {
			return err
		}
		return h.Put("b", "2")
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	rtx, err := db.BeginRead(ctx)
	if err != nil {
		t.Fatalf("BeginRead: %v", err)
	}
	defer rtx.Rollback()

	h, err := tks.OpenReadOnly(rtx)
	if err != nil {
		t.Fatalf("OpenReadOnly(ReadTx): %v", err)
	}
	if v, err := h.Get("a"); err != nil || v != "1" {
		t.Fatalf(`Get("a") = (%q, %v), want ("1", nil)`, v, err)
	}

	// Commit a write after the snapshot was pinned: overwrite one key,
	// add another. The snapshot handle must keep serving the old view.
	if err := db.Update(ctx, func(tx *gmdb.Tx) error {
		h2, err := tks.Open(tx)
		if err != nil {
			return err
		}
		if err := h2.Put("a", "changed"); err != nil {
			return err
		}
		return h2.Put("c", "3")
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if v, err := h.Get("a"); err != nil || v != "1" {
		t.Fatalf(`snapshot Get("a") after concurrent commit = (%q, %v), want ("1", nil)`, v, err)
	}
	if _, err := h.Get("c"); !errors.Is(err, gmdb.ErrNotFound) {
		t.Fatalf(`snapshot Get("c") = %v, want ErrNotFound`, err)
	}
	got := maps.Collect(h.All())
	if len(got) != 2 || got["a"] != "1" || got["b"] != "2" {
		t.Fatalf("All over snapshot = %v, want map[a:1 b:2]", got)
	}

	if err := h.Put("x", "y"); !errors.Is(err, gmdb.ErrReadOnly) {
		t.Fatalf("Put = %v, want ErrReadOnly", err)
	}
	if err := h.Delete("a"); !errors.Is(err, gmdb.ErrReadOnly) {
		t.Fatalf("Delete = %v, want ErrReadOnly", err)
	}
	if _, err := h.DeleteRange(nil, nil); !errors.Is(err, gmdb.ErrReadOnly) {
		t.Fatalf("DeleteRange = %v, want ErrReadOnly", err)
	}
	c := h.Cursor()
	if _, _, ok := c.First(); !ok {
		t.Fatalf("Cursor.First: not positioned (Err %v)", c.Err())
	}
	if err := c.Delete(); !errors.Is(err, gmdb.ErrReadOnly) {
		t.Fatalf("Cursor.Delete = %v, want ErrReadOnly", err)
	}
	c.Close()

	if err := rtx.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if _, err := h.Get("a"); !errors.Is(err, gmdb.ErrTxClosed) {
		t.Fatalf("Get after ReadTx close = %v, want ErrTxClosed", err)
	}
}

// TestSetKeyspaceSnapshotReadOnly pins the typed set-keyspace read path
// over a snapshot read transaction: membership reads observe the pinned
// snapshot (a commit after BeginRead stays invisible), every mutator
// returns gmdb.ErrReadOnly, and the handle dies with the ReadTx.
func TestSetKeyspaceSnapshotReadOnly(t *testing.T) {
	ctx := context.Background()
	db := openWith(t, ctx, tmpPath(t), gmdb.Options{PageSize: 4096, MinSize: 16, MaxSize: 256})
	defer db.Close()

	tsk := NewSetKeyspace("sks", StringEncoder{}, StringEncoder{}, nil)
	if err := db.Update(ctx, func(tx *gmdb.Tx) error {
		h, err := tsk.Create(tx)
		if err != nil {
			return err
		}
		if _, err := h.Put("k", "v1"); err != nil {
			return err
		}
		_, err = h.Put("k", "v2")
		return err
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	rtx, err := db.BeginRead(ctx)
	if err != nil {
		t.Fatalf("BeginRead: %v", err)
	}
	defer rtx.Rollback()

	h, err := tsk.OpenReadOnly(rtx)
	if err != nil {
		t.Fatalf("OpenReadOnly(ReadTx): %v", err)
	}
	if ok, err := h.Has("k"); err != nil || !ok {
		t.Fatalf("Has = (%v, %v), want (true, nil)", ok, err)
	}
	if ok, err := h.HasValue("k", "v2"); err != nil || !ok {
		t.Fatalf("HasValue = (%v, %v), want (true, nil)", ok, err)
	}

	// Commit a member after the snapshot was pinned: the handle must
	// keep serving the old view.
	if err := db.Update(ctx, func(tx *gmdb.Tx) error {
		h2, err := tsk.Open(tx)
		if err != nil {
			return err
		}
		_, err = h2.Put("k", "v3")
		return err
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if n, err := h.CountValues("k"); err != nil || n != 2 {
		t.Fatalf("CountValues after concurrent commit = (%d, %v), want (2, nil)", n, err)
	}
	if ok, err := h.HasValue("k", "v3"); err != nil || ok {
		t.Fatalf(`HasValue("v3") over snapshot = (%v, %v), want (false, nil)`, ok, err)
	}

	if _, err := h.Put("k", "v4"); !errors.Is(err, gmdb.ErrReadOnly) {
		t.Fatalf("Put = %v, want ErrReadOnly", err)
	}
	if err := h.Delete("k"); !errors.Is(err, gmdb.ErrReadOnly) {
		t.Fatalf("Delete = %v, want ErrReadOnly", err)
	}
	if err := h.DeleteValue("k", "v1"); !errors.Is(err, gmdb.ErrReadOnly) {
		t.Fatalf("DeleteValue = %v, want ErrReadOnly", err)
	}
	if _, err := h.DeleteRange(nil, nil); !errors.Is(err, gmdb.ErrReadOnly) {
		t.Fatalf("DeleteRange = %v, want ErrReadOnly", err)
	}
	c := h.Cursor()
	if _, _, ok := c.First(); !ok {
		t.Fatalf("SetCursor.First: not positioned (Err %v)", c.Err())
	}
	if err := c.Delete(); !errors.Is(err, gmdb.ErrReadOnly) {
		t.Fatalf("SetCursor.Delete = %v, want ErrReadOnly", err)
	}
	c.Close()

	// Close pins on a fresh cursor: c above already latched the
	// ErrReadOnly from its Delete, and Err() reports the first
	// latched error.
	c2 := h.Cursor()
	if _, _, ok := c2.First(); !ok {
		t.Fatalf("second SetCursor.First: not positioned (Err %v)", c2.Err())
	}
	c2.Close()
	if _, _, ok := c2.First(); ok {
		t.Fatal("SetCursor.First after Close: ok=true, want closed cursor")
	}
	if err := c2.Err(); !errors.Is(err, gmdb.ErrCursorClosed) {
		t.Fatalf("SetCursor.Err after Close = %v, want ErrCursorClosed", err)
	}

	if err := rtx.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if _, err := h.Has("k"); !errors.Is(err, gmdb.ErrTxClosed) {
		t.Fatalf("Has after ReadTx close = %v, want ErrTxClosed", err)
	}
}

// TestKeyspaceSnapshotIndexLookup pins index lookups through a handle
// opened from a snapshot read transaction (typed-keyspaces.md
// §Snapshot reads: index lookups against stored entries still work —
// no declarations supplied at open).
func TestKeyspaceSnapshotIndexLookup(t *testing.T) {
	ctx := context.Background()
	db := openWith(t, ctx, tmpPath(t), gmdb.Options{PageSize: 4096, MinSize: 16, MaxSize: 256})
	defer db.Close()

	tks := NewKeyspace[uint64, string]("users", Uint64Encoder{}, StringEncoder{})
	byFirst := &Index[uint64, string, string]{
		Name:    "by_first",
		IKEnc:   StringEncoder{},
		Extract: firstLetterIK,
	}
	if err := db.Update(ctx, func(tx *gmdb.Tx) error {
		h, err := tks.Create(tx, byFirst)
		if err != nil {
			return err
		}
		for id, name := range map[uint64]string{1: "alice", 2: "amy", 3: "bob"} {
			if err := h.Put(id, name); err != nil {
				return err
			}
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
		ih, err := h.Index("by_first")
		if err != nil {
			t.Fatalf("Index: %v", err)
		}
		q := NewIndexQuery[uint64, string, string](ih, StringEncoder{})
		got := maps.Collect(q.Lookup("a"))
		if err := q.Err(); err != nil {
			t.Fatalf("Lookup Err: %v", err)
		}
		if len(got) != 2 || got[1] != "alice" || got[2] != "amy" {
			t.Fatalf("index lookup over snapshot = %v, want map[1:alice 2:amy]", got)
		}
		return nil
	}); err != nil {
		t.Fatalf("View: %v", err)
	}
}

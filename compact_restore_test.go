package gmdb

import (
	"context"
	"errors"
	"os"
	"sync/atomic"
	"testing"
)

// TestCompactRenameRefusalRestoresHandle pins the publish-refusal
// contract (api-surface.md §Check, CopyTo, Compact): when the publish
// rename is refused — the windows sole-mapper gate, forced here via
// the rename seam so the path runs on every platform — Compact
// returns a clean error wrapping the refusal, the handle is NOT
// poisoned, the temp is removed, the pager was already torn down at
// rename time (teardown-before-rename), and the restored handle
// serves reads and writes of the original database. A subsequent
// Compact with the refusal gone succeeds.
func TestCompactRenameRefusalRestoresHandle(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)
	db, err := Open(ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 4096})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	buildChurnedDB(t, db)

	renameErr := errors.New("simulated sole-mapper refusal")
	var pgrNilAtRename atomic.Bool
	hook := func(oldname, newname string) error {
		db.mu.Lock()
		pgrNilAtRename.Store(db.pgr == nil)
		db.mu.Unlock()
		return renameErr
	}
	compactRenameHookForTest.Store(&hook)
	defer compactRenameHookForTest.Store(nil)

	// Simulate an uncovered dead-peer writeback lineage (the
	// covered-through mark trailing the takeover sequence): the
	// restore must run the same covered-through gate Open's live-join
	// arm runs, or a handle that Compacts (never Begins) after a peer
	// death would leave the lineage uncovered for its lifetime.
	seq := db.coord.TakeoverSeq()
	db.coord.SetRedirtyCoveredSeq(seq + 1) // any value != seq opens the gate

	if err := db.Compact(); !errors.Is(err, renameErr) {
		t.Fatalf("Compact = %v, want error wrapping the refusal", err)
	}
	if got := db.coord.RedirtyCoveredSeq(); got != seq {
		t.Errorf("covered-through mark = %d after restore, want %d — "+
			"the restore skipped the dropped-writeback lineage cover", got, seq)
	}
	if !pgrNilAtRename.Load() {
		t.Error("pager still installed at rename time — teardown-before-rename violated")
	}
	if db.poisoned.Load() {
		t.Fatal("handle poisoned by a retryable rename refusal")
	}
	if _, serr := os.Stat(path + ".compact"); !os.IsNotExist(serr) {
		t.Errorf("compact temp not removed after refusal (stat err: %v)", serr)
	}

	// The restored handle serves the original data...
	rtx, err := db.BeginRead(ctx)
	if err != nil {
		t.Fatalf("BeginRead after restore: %v", err)
	}
	rks, err := rtx.OpenKeyspaceReadOnly("k")
	if err != nil {
		t.Fatalf("OpenKeyspace after restore: %v", err)
	}
	if _, err := rks.Get([]byte("key000000")); err != nil {
		t.Errorf("surviving key unreadable after restore: %v", err)
	}
	_ = rtx.Rollback()

	// ...and accepts writes.
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin after restore: %v", err)
	}
	ks, err := tx.OpenKeyspace("k")
	if err != nil {
		t.Fatal(err)
	}
	if err := ks.Put([]byte("post-restore"), []byte("v")); err != nil {
		t.Fatalf("Put after restore: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit after restore: %v", err)
	}

	// With the refusal gone, Compact completes and the write survives.
	compactRenameHookForTest.Store(nil)
	if err := db.Compact(); err != nil {
		t.Fatalf("Compact after restore: %v", err)
	}
	if err := db.View(ctx, func(rtx *ReadTx) error {
		rks, err := rtx.OpenKeyspaceReadOnly("k")
		if err != nil {
			return err
		}
		_, err = rks.Get([]byte("post-restore"))
		return err
	}); err != nil {
		t.Errorf("post-restore key lost across the later Compact: %v", err)
	}
}

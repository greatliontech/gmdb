package gmdb

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"iter"
	"testing"

	"github.com/greatliontech/gmdb/internal/btree"
	"github.com/greatliontech/gmdb/internal/page"
	"github.com/greatliontech/gmdb/internal/verify"
)

// Post-iteration error surface (api-surface.md §Range Iterators):
// Keyspace.Err / SetKeyspace.Err report how the LAST All / Range /
// Prefix sequence on the handle ended — nil on every clean end
// (exhaustion, a Range end bound, a Prefix mismatch, a caller break),
// the truncating cursor error otherwise. The error resets when the
// next sequence's iteration starts, and an error-truncated sequence
// never yields the phantom (key, nil) pair the cursor's
// overflow-value miss channel would otherwise surface.

func iterErrFixture(t *testing.T) (*Tx, *Keyspace) {
	t.Helper()
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 256,
		Maintenance: MaintenanceOptions{Disable: true}})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	tx, _ := db.Begin(ctx)
	ks, err := tx.CreateKeyspace("k")
	if err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	return tx, ks
}

func TestIteratorErrNilOnCleanEnds(t *testing.T) {
	_, ks := iterErrFixture(t)
	if err := ks.Err(); err != nil {
		t.Fatalf("Err() before any iteration = %v, want nil", err)
	}
	for range ks.All() { // empty keyspace: exhaustion via First() == nil
	}
	if err := ks.Err(); err != nil {
		t.Fatalf("Err() after empty All = %v, want nil", err)
	}
	for i := range 10 {
		if err := ks.Put(fmt.Appendf(nil, "k%02d", i), []byte("v")); err != nil {
			t.Fatal(err)
		}
	}
	if err := ks.Put([]byte("zz"), []byte("v")); err != nil { // past every k0* prefix
		t.Fatal(err)
	}
	n := 0
	for range ks.All() {
		n++
	}
	if n != 11 {
		t.Fatalf("All yielded %d, want 11", n)
	}
	if err := ks.Err(); err != nil {
		t.Fatalf("Err() after complete All = %v, want nil", err)
	}
	for range ks.Range([]byte("k02"), []byte("k05")) { // clean end-bound exit
	}
	if err := ks.Err(); err != nil {
		t.Fatalf("Err() after Range bound exit = %v, want nil", err)
	}
	n = 0
	for range ks.Prefix([]byte("k0")) { // clean prefix-MISMATCH exit at "zz"
		n++
	}
	if n != 10 {
		t.Fatalf("Prefix yielded %d, want 10", n)
	}
	if err := ks.Err(); err != nil {
		t.Fatalf("Err() after Prefix mismatch exit = %v, want nil", err)
	}
	for range ks.All() {
		break // caller break is a clean end
	}
	if err := ks.Err(); err != nil {
		t.Fatalf("Err() after caller break = %v, want nil", err)
	}
}

func TestSetIteratorErrNilOnCleanEnds(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 256,
		Maintenance: MaintenanceOptions{Disable: true}})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tx, _ := db.Begin(ctx)
	sks, err := tx.CreateSetKeyspace("s", nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = tx
	for range sks.All() {
	}
	if err := sks.Err(); err != nil {
		t.Fatalf("Err() after empty All = %v, want nil", err)
	}
	for _, k := range []string{"a", "b"} {
		for _, m := range []string{"m1", "m2"} {
			if _, err := sks.Put([]byte(k), []byte(m)); err != nil {
				t.Fatal(err)
			}
		}
	}
	n := 0
	for range sks.All() {
		n++
	}
	if n != 4 {
		t.Fatalf("All yielded %d member pairs, want 4", n)
	}
	if err := sks.Err(); err != nil {
		t.Fatalf("Err() after complete All = %v, want nil", err)
	}
	for range sks.Prefix([]byte("a")) {
		break
	}
	if err := sks.Err(); err != nil {
		t.Fatalf("Err() after caller break = %v, want nil", err)
	}
}

// A loop-body mutation on the same keyspace stales the sequence's
// cursor and ENDS the sequence; the truncation is observable as
// Err() == ErrCursorStale, and the next sequence resets it.
func TestIteratorErrReportsLoopBodyMutationStale(t *testing.T) {
	_, ks := iterErrFixture(t)
	for i := range 10 {
		if err := ks.Put(fmt.Appendf(nil, "k%02d", i), []byte("v")); err != nil {
			t.Fatal(err)
		}
	}
	n := 0
	for range ks.All() {
		n++
		if err := ks.Put([]byte("zz"), []byte("v")); err != nil {
			t.Fatal(err)
		}
	}
	if n != 1 {
		t.Fatalf("stale-truncated All yielded %d, want 1", n)
	}
	if err := ks.Err(); !errors.Is(err, ErrCursorStale) {
		t.Fatalf("Err() after loop-body mutation = %v, want ErrCursorStale", err)
	}
	// The reset happens at the START of the next sequence, so even a
	// caller-break end (which skips the post-loop capture) must not
	// leak the prior sequence's error.
	for range ks.All() {
		break
	}
	if err := ks.Err(); err != nil {
		t.Fatalf("Err() after post-error break sequence = %v, want nil (per-sequence reset)", err)
	}
	// Fresh sequence resets: a clean full iteration leaves Err() nil.
	n = 0
	for range ks.All() {
		n++
	}
	if n != 11 {
		t.Fatalf("fresh All yielded %d, want 11", n)
	}
	if err := ks.Err(); err != nil {
		t.Fatalf("Err() after fresh clean All = %v, want nil", err)
	}
}

func TestSetIteratorErrReportsLoopBodyMutationStale(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 256,
		Maintenance: MaintenanceOptions{Disable: true}})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tx, _ := db.Begin(ctx)
	_ = tx
	sks, err := tx.CreateSetKeyspace("s", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"a", "b", "c"} {
		if _, err := sks.Put([]byte(k), []byte("m")); err != nil {
			t.Fatal(err)
		}
	}
	n := 0
	for range sks.All() {
		n++
		if _, err := sks.Put([]byte("z"), []byte("m")); err != nil {
			t.Fatal(err)
		}
	}
	if n != 1 {
		t.Fatalf("stale-truncated All yielded %d, want 1", n)
	}
	if err := sks.Err(); !errors.Is(err, ErrCursorStale) {
		t.Fatalf("Err() after loop-body mutation = %v, want ErrCursorStale", err)
	}
	n = 0
	for range sks.All() {
		n++
	}
	if n != 4 {
		t.Fatalf("fresh All yielded %d, want 4", n)
	}
	if err := sks.Err(); err != nil {
		t.Fatalf("Err() after fresh clean All = %v, want nil", err)
	}
}

// A structural read fault (bitrotted root page) truncates every
// iterator surface at the first descent, and Err() surfaces the
// public ErrBadPageChecksum sentinel — previously indistinguishable
// from an empty keyspace.
func TestIteratorErrReportsBadPageChecksum(t *testing.T) {
	ctx := context.Background()
	path := corruptRootChecksumDB(t, ctx)
	db, err := Open(ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 256})
	if err != nil {
		t.Fatalf("re-Open: %v", err)
	}
	defer db.Close()
	tx, _ := db.Begin(ctx)
	defer tx.Rollback()
	ks, err := tx.OpenKeyspace("k")
	if err != nil {
		t.Fatalf("OpenKeyspace: %v", err)
	}
	for name, seq := range map[string]func() int{
		"All": func() int {
			n := 0
			for range ks.All() {
				n++
			}
			return n
		},
		"Range": func() int {
			n := 0
			for range ks.Range([]byte("key00000"), nil) {
				n++
			}
			return n
		},
		"Prefix": func() int {
			n := 0
			for range ks.Prefix([]byte("key")) {
				n++
			}
			return n
		},
	} {
		if n := seq(); n != 0 {
			t.Fatalf("%s over bitrotted root yielded %d pairs, want 0", name, n)
		}
		if err := ks.Err(); !errors.Is(err, ErrBadPageChecksum) {
			t.Fatalf("%s: Err() = %v, want ErrBadPageChecksum", name, err)
		}
	}
}

// The SetKeyspace structural-fault path differs from Keyspace's: a
// SetCursor read fault latches closeErr and ends iteration with a nil
// key (never a phantom pair), and the post-loop capture must surface
// that closeErr through SetKeyspace.Err().
func TestSetIteratorErrReportsBadPageChecksum(t *testing.T) {
	const pageSize = 4096
	ctx := context.Background()
	path := tmpPath(t)
	db, err := Open(ctx, path, Options{PageSize: pageSize, MinSize: 16, MaxSize: 256})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	tx, _ := db.Begin(ctx)
	sks, err := tx.CreateSetKeyspace("s", nil)
	if err != nil {
		t.Fatalf("CreateSetKeyspace: %v", err)
	}
	for i := range 400 {
		if _, err := sks.Put(fmt.Appendf(nil, "key%05d", i), []byte("m")); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	rtx, _ := db.Begin(ctx)
	rsks, err := rtx.OpenSetKeyspace("s")
	if err != nil {
		t.Fatalf("OpenSetKeyspace: %v", err)
	}
	root := rsks.desc.Root
	rtx.Rollback()
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Flip a byte in the outer tree root's XXH3-64 footer.
	corruptFileByte(t, path, int64(root)*pageSize+pageSize-4, 0xFF)

	db2, err := Open(ctx, path, Options{PageSize: pageSize, MinSize: 16, MaxSize: 256})
	if err != nil {
		t.Fatalf("re-Open: %v", err)
	}
	defer db2.Close()
	tx2, _ := db2.Begin(ctx)
	defer tx2.Rollback()
	sks2, err := tx2.OpenSetKeyspace("s")
	if err != nil {
		t.Fatalf("OpenSetKeyspace: %v", err)
	}
	for name, iterate := range map[string]func() int{
		"All": func() int {
			n := 0
			for range sks2.All() {
				n++
			}
			return n
		},
		"Range": func() int {
			n := 0
			for range sks2.Range([]byte("key00000"), nil) {
				n++
			}
			return n
		},
		"Prefix": func() int {
			n := 0
			for range sks2.Prefix([]byte("key")) {
				n++
			}
			return n
		},
	} {
		if n := iterate(); n != 0 {
			t.Fatalf("%s over bitrotted root yielded %d pairs, want 0", name, n)
		}
		if err := sks2.Err(); !errors.Is(err, ErrBadPageChecksum) {
			t.Fatalf("%s: Err() = %v, want ErrBadPageChecksum", name, err)
		}
	}
}

// An overflow-value assembly failure mid-sequence must truncate the
// sequence at the failing row WITHOUT yielding the phantom (key, nil)
// pair the cursor's miss channel produces, and Err() must carry the
// fault. Rows before the failing one yield normally.
func TestIteratorErrNoPhantomPairOnCorruptOverflowValue(t *testing.T) {
	const pageSize = 4096
	ctx := context.Background()
	path := tmpPath(t)
	db, err := Open(ctx, path, Options{PageSize: pageSize, MinSize: 16, MaxSize: 1 << 16})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ks, err := tx.CreateKeyspace("k")
	if err != nil {
		t.Fatal(err)
	}
	big := bytes.Repeat([]byte{0xAB}, 9000) // 3-page overflow run
	for _, kv := range []struct{ k, v []byte }{
		{[]byte("a"), []byte("small")},
		{[]byte("big"), big},
		{[]byte("z"), []byte("small")},
	} {
		if err := ks.Put(kv.k, kv.v); err != nil {
			t.Fatalf("Put(%q): %v", kv.k, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	// Locate the run head via the committed tree, then corrupt a
	// follower's extent byte (whole-run digest cover).
	rtx, _ := db.Begin(ctx)
	rks, _ := rtx.OpenKeyspace("k")
	root := rks.desc.Root
	rtx.Rollback()
	head := overflowHeadOf(t, db, root)
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	corruptFileByte(t, path, int64(head)*pageSize+pageSize+100, 0x01)

	db2, err := Open(ctx, path, Options{PageSize: pageSize, MinSize: 16, MaxSize: 1 << 16})
	if err != nil {
		t.Fatalf("re-Open: %v", err)
	}
	defer db2.Close()
	tx2, _ := db2.BeginRead(ctx)
	defer tx2.Rollback()
	ks2, err := tx2.OpenKeyspaceReadOnly("k")
	if err != nil {
		t.Fatalf("OpenKeyspaceReadOnly: %v", err)
	}
	// All three surfaces carry their own copy of the phantom guard;
	// each must truncate at the failing row without the (key, nil)
	// yield and record the fault.
	for name, seq := range map[string]iter.Seq2[[]byte, []byte]{
		"All":    ks2.All(),
		"Range":  ks2.Range([]byte("a"), nil),
		"Prefix": ks2.Prefix(nil),
	} {
		var got []string
		for k, v := range seq {
			if v == nil {
				t.Fatalf("%s: phantom (key=%q, nil) pair yielded", name, k)
			}
			got = append(got, string(k))
		}
		if len(got) != 1 || got[0] != "a" {
			t.Fatalf("%s over corrupt overflow yielded %q, want [a] (truncate at the failing row)", name, got)
		}
		if err := ks2.Err(); !errors.Is(err, ErrBadPageChecksum) {
			t.Fatalf("%s: Err() = %v, want ErrBadPageChecksum", name, err)
		}
	}
}

// A child-transaction freeze truncates an in-flight sequence. The
// cascade's ErrChildActive leg alone is transient: after the child
// resolves, the per-sequence slot still reports ErrChildActive until
// the next sequence resets it (api-surface.md §Range Iterators).
func TestIteratorErrReportsChildFreezeTruncation(t *testing.T) {
	tx, ks := iterErrFixture(t)
	for i := range 5 {
		if err := ks.Put(fmt.Appendf(nil, "k%02d", i), []byte("v")); err != nil {
			t.Fatal(err)
		}
	}
	var child *Tx
	n := 0
	for range ks.All() {
		n++
		if child == nil {
			c, err := tx.BeginChild()
			if err != nil {
				t.Fatalf("BeginChild: %v", err)
			}
			child = c
		}
	}
	if n != 1 {
		t.Fatalf("freeze-truncated All yielded %d, want 1", n)
	}
	if err := ks.Err(); !errors.Is(err, ErrChildActive) {
		t.Fatalf("Err() during freeze = %v, want ErrChildActive", err)
	}
	if err := child.Rollback(); err != nil {
		t.Fatalf("child Rollback: %v", err)
	}
	if err := ks.Err(); !errors.Is(err, ErrChildActive) {
		t.Fatalf("Err() after child resolved = %v, want ErrChildActive (per-sequence slot, not the transient cascade leg)", err)
	}
	n = 0
	for range ks.All() {
		n++
	}
	if n != 5 {
		t.Fatalf("fresh All yielded %d, want 5", n)
	}
	if err := ks.Err(); err != nil {
		t.Fatalf("Err() after fresh clean All = %v, want nil", err)
	}
}

// The Err cascade mirrors IndexHandle.Err: broader handle truths win
// over the sticky per-sequence error.
func TestIteratorErrHandleCascade(t *testing.T) {
	tx, ks := iterErrFixture(t)
	if err := ks.Put([]byte("a"), []byte("v")); err != nil {
		t.Fatal(err)
	}
	ks2, err := tx.CreateKeyspace("k2")
	if err != nil {
		t.Fatalf("CreateKeyspace(k2): %v", err)
	}
	if err := tx.DeleteKeyspace("k"); err != nil {
		t.Fatalf("DeleteKeyspace: %v", err)
	}
	if err := ks.Err(); !errors.Is(err, ErrKeyspaceClosed) {
		t.Fatalf("Err() on dead handle = %v, want ErrKeyspaceClosed", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	// Dead wins over tx-closed (the IndexHandle.Err order); a live
	// handle on the closed tx reports ErrTxClosed.
	if err := ks.Err(); !errors.Is(err, ErrKeyspaceClosed) {
		t.Fatalf("Err() on dead handle after Rollback = %v, want ErrKeyspaceClosed", err)
	}
	if err := ks2.Err(); !errors.Is(err, ErrTxClosed) {
		t.Fatalf("Err() on closed tx = %v, want ErrTxClosed", err)
	}
}

// overflowHeadOf returns the first overflow run head reachable from
// root, via the raw page reader (test-only walk; mirrors
// putOverflowValue's head discovery).
func overflowHeadOf(t *testing.T, db *DB, root uint64) uint64 {
	t.Helper()
	var head uint64
	if err := btree.WalkLeafEntries(verify.RawPageReader{P: db.pgr}, db.pgr.Config(), root, db.pgr.HighWaterMark(), func(e page.LeafEntry) error {
		if e.IsOverflow() && head == 0 {
			head = e.OverflowPage
		}
		return nil
	}); err != nil {
		t.Fatalf("WalkLeafEntries: %v", err)
	}
	if head == 0 {
		t.Fatal("no overflow run found — value did not promote to overflow")
	}
	return head
}

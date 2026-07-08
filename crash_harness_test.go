package gmdb

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/thegrumpylion/gmdb/internal/pager"
)

// Crash-consistency harness. A crashRecorder installed on the DB writer's
// FileOps seam logs every pwrite + truncate (in order) and the write-count
// at each committed boundary. Replaying the trace onto the pre-workload
// image synthesizes the exact on-disk bytes a crash (power loss) would
// leave at any point — the durable image after commit K, or a torn image
// mid-commit. Reopening those images and asserting durability.md's
// recovery invariants turns the spec's crash reasoning into executable
// tests, replacing the optimistic crashCopy (which only captures the
// all-writes-survived image).

type crashOpKind int

const (
	crashOpWrite crashOpKind = iota
	crashOpTruncate
)

type crashOp struct {
	kind crashOpKind
	off  int64
	data []byte // owned copy, for writes
	size int64  // for truncates
}

type crashRecorder struct {
	inner pager.FileOps
	mu    sync.Mutex
	ops   []crashOp
	marks []int // len(ops) captured at each committed boundary
}

func (r *crashRecorder) WriteAt(p []byte, off int64) (int, error) {
	r.mu.Lock()
	buf := make([]byte, len(p))
	copy(buf, p)
	r.ops = append(r.ops, crashOp{kind: crashOpWrite, off: off, data: buf})
	r.mu.Unlock()
	return r.inner.WriteAt(p, off)
}

func (r *crashRecorder) ReadAt(p []byte, off int64) (int, error) { return r.inner.ReadAt(p, off) }

func (r *crashRecorder) Truncate(size int64) error {
	r.mu.Lock()
	r.ops = append(r.ops, crashOp{kind: crashOpTruncate, size: size})
	r.mu.Unlock()
	return r.inner.Truncate(size)
}

func (r *crashRecorder) Fdatasync() error { return r.inner.Fdatasync() }

// mark records a committed boundary: ops[:mark] is the fully-durable state
// a SyncDurable commit left when Commit returned (step-4 fdatasync done).
func (r *crashRecorder) mark() {
	r.mu.Lock()
	r.marks = append(r.marks, len(r.ops))
	r.mu.Unlock()
}

// synthImage applies ops[:n] to a copy of initial — the on-disk bytes a
// crash would leave after exactly those operations.
func synthImage(initial []byte, ops []crashOp, n int) []byte {
	img := make([]byte, len(initial))
	copy(img, initial)
	for _, op := range ops[:n] {
		switch op.kind {
		case crashOpWrite:
			end := op.off + int64(len(op.data))
			if int64(len(img)) < end {
				img = append(img, make([]byte, end-int64(len(img)))...)
			}
			copy(img[op.off:end], op.data)
		case crashOpTruncate:
			switch {
			case op.size < int64(len(img)):
				img = img[:op.size]
			case op.size > int64(len(img)):
				img = append(img, make([]byte, op.size-int64(len(img)))...)
			}
		}
	}
	return img
}

func crashKey(i int) []byte { return fmt.Appendf(nil, "key%06d", i) }
func crashVal(i int) []byte { return fmt.Appendf(nil, "val-%06d-payload", i) }

// TestCrashAckedDurableCommitsAlwaysRecover pins the core no-data-loss
// property (durability.md §Durability Modes, SyncDurable): after ANY
// acknowledged SyncDurable commit, a crash at that instant leaves a
// database that reopens with every acked commit's data intact and Check()
// clean. Each commit's completion image is synthesized from the recorded
// write trace (all writes are step-4-fsynced before Commit returns) and
// reopened.
func TestCrashAckedDurableCommitsAlwaysRecover(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)
	opts := Options{PageSize: 4096, MinSize: 16, MaxSize: 512, SyncMode: SyncDurable, Maintenance: MaintenanceOptions{Disable: true}}
	db, err := Open(ctx, path, opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	rec := &crashRecorder{inner: db.WriterFileOpsForTest()}
	restore := db.SetWriterFileOpsForTest(rec)

	initial, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read initial image: %v", err)
	}

	const commits = 6
	const perCommit = 40
	for c := 0; c < commits; c++ {
		tx, err := db.Begin(ctx)
		if err != nil {
			t.Fatalf("Begin c=%d: %v", c, err)
		}
		var ks *Keyspace
		if c == 0 {
			ks, err = tx.CreateKeyspace("k")
		} else {
			ks, err = tx.OpenKeyspace("k")
		}
		if err != nil {
			t.Fatalf("keyspace c=%d: %v", c, err)
		}
		for i := c * perCommit; i < (c+1)*perCommit; i++ {
			if err := ks.Put(crashKey(i), crashVal(i)); err != nil {
				t.Fatalf("Put %d: %v", i, err)
			}
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("Commit c=%d: %v", c, err)
		}
		rec.mark()
	}
	restore()
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// For each acked commit boundary, the synthesized durable image must
	// reopen with exactly the keys acked through that commit.
	for m := 0; m < commits; m++ {
		wantKeys := (m + 1) * perCommit
		img := synthImage(initial, rec.ops, rec.marks[m])
		verifyDurableCrashImage(t, opts, img, fmt.Sprintf("after-commit-%d", m), wantKeys, -1)
	}
}

// verifyDurableCrashImage reopens a synthesized crash image and asserts it
// is a fully-consistent database: Open succeeds, Check() reports no
// error-or-worse issue, and keys [0,wantKeys) are all present with correct
// values (nothing acked-durable was lost or corrupted). When absentFrom >=
// 0, it additionally asserts key absentFrom is NOT present — used by the
// torn-meta case to prove recovery fell back to the durable epoch rather
// than accepting the torn (post-epoch) meta.
func verifyDurableCrashImage(t *testing.T, opts Options, img []byte, label string, wantKeys, absentFrom int) {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "crash.gmdb")
	if err := os.WriteFile(p, img, 0o600); err != nil {
		t.Fatalf("[%s] write image: %v", label, err)
	}
	ctx := context.Background()
	db, err := Open(ctx, p, opts)
	if err != nil {
		t.Fatalf("[%s] Open crash image (want %d keys): %v", label, wantKeys, err)
	}
	defer db.Close()

	for _, iss := range collectIssues(db.Check()) {
		if iss.Severity != CheckWarning {
			t.Errorf("[%s] Check: code=%s sev=%d page=%d msg=%s", label, iss.Code, iss.Severity, iss.PageID, iss.Message)
		}
	}

	rtx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("[%s] Begin on crash image: %v", label, err)
	}
	defer rtx.Rollback()
	ks, err := rtx.OpenKeyspace("k")
	if err != nil {
		t.Fatalf("[%s] OpenKeyspace on crash image: %v", label, err)
	}
	for i := 0; i < wantKeys; i++ {
		got, err := ks.Get(crashKey(i))
		if err != nil {
			t.Fatalf("[%s] Get key %d (of %d acked): %v — acked-durable data lost", label, i, wantKeys, err)
		}
		if !bytes.Equal(got, crashVal(i)) {
			t.Fatalf("[%s] key %d = %q, want %q — acked-durable data corrupted", label, i, got, crashVal(i))
		}
	}
	if absentFrom >= 0 {
		if _, err := ks.Get(crashKey(absentFrom)); !errors.Is(err, ErrNotFound) {
			t.Fatalf("[%s] key %d present (err=%v), want ErrNotFound — recovery accepted a post-epoch (torn) meta instead of falling back", label, absentFrom, err)
		}
	}
}

// TestCrashMidInflightCommitPreservesDurableEpoch pins the SyncDurable
// commit protocol's crash safety (durability.md §Recovery, pager-slab.md
// commit ordering): crashing at ANY point during an in-flight commit must
// leave the previous durable epoch fully intact — the new commit is either
// wholly present or wholly absent, never a partial or silently-wrong
// state. Two crash shapes over the final commit's write region:
//
//	(a) an ordered prefix ending at every write boundary (crash between any
//	    two writes) — data pages precede the meta publish, so before the
//	    meta lands the epoch is unchanged;
//	(b) a TORN meta write (the step-3 single-page meta pwrite half-lands) —
//	    the xxhash64 footer rejects it and recovery falls back to the other
//	    slot (durability.md §Checkpoints single-meta-slot atomicity).
func TestCrashMidInflightCommitPreservesDurableEpoch(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)
	opts := Options{PageSize: 4096, MinSize: 16, MaxSize: 512, SyncMode: SyncDurable, Maintenance: MaintenanceOptions{Disable: true}}
	db, err := Open(ctx, path, opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	rec := &crashRecorder{inner: db.WriterFileOpsForTest()}
	restore := db.SetWriterFileOpsForTest(rec)
	initial, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read initial image: %v", err)
	}

	const commits = 6
	const perCommit = 40
	for c := 0; c < commits; c++ {
		tx, err := db.Begin(ctx)
		if err != nil {
			t.Fatalf("Begin c=%d: %v", c, err)
		}
		var ks *Keyspace
		if c == 0 {
			ks, err = tx.CreateKeyspace("k")
		} else {
			ks, err = tx.OpenKeyspace("k")
		}
		if err != nil {
			t.Fatalf("keyspace c=%d: %v", c, err)
		}
		for i := c * perCommit; i < (c+1)*perCommit; i++ {
			if err := ks.Put(crashKey(i), crashVal(i)); err != nil {
				t.Fatalf("Put %d: %v", i, err)
			}
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("Commit c=%d: %v", c, err)
		}
		rec.mark()
	}
	restore()
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// The last acked epoch before the in-flight commit is commit K.
	K := commits - 2
	durableKeys := (K + 1) * perCommit
	lo, hi := rec.marks[K], rec.marks[commits-1]

	// (a) crash at every write boundary within the in-flight commit.
	for j := lo; j <= hi; j++ {
		img := synthImage(initial, rec.ops, j)
		verifyDurableCrashImage(t, opts, img, fmt.Sprintf("inflight-prefix-%d", j-lo), durableKeys, -1)
	}

	// (b) torn step-3 meta write. The meta payload (checksum over the
	// leading bytes, stored just after) is a small contiguous region at the
	// page start — durability.md §Checkpoints notes a tear either misses it
	// wholly (payload untouched, old valid meta survives) or lands within it
	// (checksum mismatch). Either way recovery must NOT adopt the in-flight
	// commit: it falls back to the durable epoch. Sweep prefix-tear lengths
	// that land within the payload (metaOffChecksum is 232-ish; these are
	// all below it, so the persisted checksum is stale relative to content).
	pageSize := int64(4096)
	metaIdx := -1
	for j := lo; j < hi; j++ {
		if rec.ops[j].kind == crashOpWrite && rec.ops[j].off < 2*pageSize {
			metaIdx = j
		}
	}
	if metaIdx < 0 {
		t.Fatal("no meta pwrite found in the in-flight commit region")
	}
	op := rec.ops[metaIdx]
	// Tear lengths within the checksummed prefix [0, metaOffChecksum): the
	// checksum slot at metaOffChecksum is never reached, so the persisted
	// checksum is stale relative to the (partly-new) content — invalid, or
	// (tear 0 / all-identical prefix) the old valid meta survives. Both
	// force a fallback below the in-flight epoch.
	metaOff := pager.MetaChecksumOffsetForTest()
	for _, tear := range []int{0, 8, metaOff / 2, metaOff - 1} {
		img := synthImage(initial, rec.ops, metaIdx) // everything up to (not incl) the meta write
		end := op.off + int64(tear)
		if int64(len(img)) < end {
			img = append(img, make([]byte, end-int64(len(img)))...)
		}
		copy(img[op.off:end], op.data[:tear]) // persist only the first `tear` bytes of the new meta
		verifyDurableCrashImage(t, opts, img, fmt.Sprintf("torn-meta-%d", tear), durableKeys, durableKeys)
	}
}

package pager

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
)

// buildLazyImage produces an opened DB whose on-disk state is NOT
// self-durable (SyncNone commits carrying the genesis epoch forward)
// so the gated recovery path takes the recovery-commit arm.
func buildLazyImage(t *testing.T) (*OpenedDB, func()) {
	t.Helper()
	f, db, cleanup := initDB(t, false)
	_ = f
	p := db.Pager
	prev, prevActive := db.Meta, db.ActiveMetaIdx
	for txn := uint64(1); txn <= 2; txn++ {
		p.BeginTx(TxParams{
			HighWaterMark: prev.HighWaterMark, MaxSize: prev.MaxSize,
			GrowStep: prev.GrowStep, MinSize: prev.MinSize, TxnID: txn,
			ReclamationBound: func() uint64 { return 0 },
		})
		id, err := p.AllocPage()
		if err != nil {
			t.Fatalf("AllocPage: %v", err)
		}
		if _, err := p.AllocSlab(id); err != nil {
			t.Fatalf("AllocSlab: %v", err)
		}
		res, err := p.Commit(CommitParams{NewTxnID: txn, Flags: prev.Flags, Sync: SyncNone}, prev, prevActive)
		if err != nil {
			t.Fatalf("Commit: %v", err)
		}
		prev, prevActive = res.Meta, res.ActiveMetaIdx
	}
	db.Meta, db.ActiveMetaIdx = prev, prevActive
	return db, cleanup
}

// TestRecoveryCommitFsyncFailurePropagatesAndRetries pins the
// recovery-commit fsync failure path (durability.md §Recovery step 5,
// idempotent-under-crash): a failing fsync surfaces the error with
// nothing anchored; a retried RecoverToDurable re-runs the whole
// sequence and succeeds. The fault is injected through the FileOps seam
// (this DB's on-disk state takes the recovery-commit arm, whose single
// fdatasync is the one the failing FileOps traps).
func TestRecoveryCommitFsyncFailurePropagatesAndRetries(t *testing.T) {
	db, cleanup := buildLazyImage(t)
	defer cleanup()
	p := db.Pager
	file := p.file

	fops := &countingFaultOps{inner: osFileOps{f: file}, failSync: true}
	restore := p.SetFileOpsForTest(fops)
	_, _, _, err := p.RecoverToDurable(file)
	restore()
	if !errors.Is(err, errInjectedIO) {
		t.Fatalf("RecoverToDurable error = %v, want the injected fsync failure", err)
	}
	if fops.syncCalls.Load() == 0 {
		t.Fatal("seam not exercised: recovery-commit fdatasync never reached the fault ops")
	}
	if got := p.AnchoredEpoch(); got != 0 {
		t.Fatalf("anchored epoch = %d after failed fsync, want 0 (nothing anchored)", got)
	}

	// Retry after the fault clears. In-process the failed attempt's
	// pwrite is already visible (page cache), so the re-read under
	// the grant selects the recovery meta and the retry takes the
	// ANCHOR arm — the equivalent end state (after a real crash the
	// pwrite is lost and the recovery-commit arm re-runs; both roads
	// end at the same durable state, which is the idempotence the
	// spec claims). Assert the STATE, not the arm.
	m, _, _, err := p.RecoverToDurable(file)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if m.TxnID != db.Meta.TxnID+1 || !m.SelfDurable() {
		t.Fatalf("retry: TxnID=%d SelfDurable=%v, want %d/true",
			m.TxnID, m.SelfDurable(), db.Meta.TxnID+1)
	}
	if got := p.AnchoredEpoch(); got != m.TxnID {
		t.Fatalf("anchored epoch = %d after retry, want %d", got, m.TxnID)
	}
}

// TestAnchorFsyncFailurePropagates pins the self-durable arm's anchor
// fsync failure: the error surfaces and the anchored epoch does not
// advance — a failed fsync must never anchor the assertion it was
// supposed to make disk-fast (durability.md §Anchoring). Genesis is
// self-durable, so RecoverToDurable takes the anchor arm and its single
// fdatasync is the one the failing FileOps traps.
func TestAnchorFsyncFailurePropagates(t *testing.T) {
	f, db, cleanup := initDB(t, false)
	_ = f
	defer cleanup()
	p := db.Pager

	fops := &countingFaultOps{inner: osFileOps{f: p.file}, failSync: true}
	restore := p.SetFileOpsForTest(fops)
	_, _, _, err := p.RecoverToDurable(p.file)
	restore()
	if !errors.Is(err, errInjectedIO) {
		t.Fatalf("RecoverToDurable error = %v, want the injected fsync failure", err)
	}
	if fops.syncCalls.Load() == 0 {
		t.Fatal("seam not exercised: anchor fdatasync never reached the fault ops")
	}

	// Retry succeeds and anchors epoch 0's assertion.
	m, _, recovered, err := p.RecoverToDurable(p.file)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if recovered || !m.SelfDurable() {
		t.Fatalf("retry: recovered=%v SelfDurable=%v, want false/true (anchor arm)", recovered, m.SelfDurable())
	}
}

// TestAnchorArmRewritesTheSlot pins the anchor REWRITE (durability.md
// §Anchoring: the byte-identical meta pwrite before the anchor fsync is
// load-bearing — a prior failed fsync both consumes the kernel error and
// marks pages clean, so a bare fdatasync would anchor an assertion the
// disk never received). The rewrite has no data-observable effect
// (identical bytes), but the FileOps seam witnesses the pwrite directly:
// a recording FileOps must see a write to the selected (self-durable)
// meta slot during the anchor arm. (This supersedes the earlier
// mtime-sentinel proxy — the seam observes the syscall itself.)
func TestAnchorArmRewritesTheSlot(t *testing.T) {
	f, db, cleanup := initDB(t, false)
	_ = f
	defer cleanup()
	p := db.Pager
	ps := int64(testPageSize)
	selectedSlot := int64(db.ActiveMetaIdx) // genesis self-durable slot

	rec := &recordingOps{inner: osFileOps{f: p.file}}
	restore := p.SetFileOpsForTest(rec)
	_, _, recovered, err := p.RecoverToDurable(p.file)
	restore()
	if err != nil || recovered {
		t.Fatalf("RecoverToDurable: recovered=%v err=%v, want anchor arm", recovered, err)
	}

	var sawRewrite bool
	for _, off := range rec.writes {
		if off/ps == selectedSlot {
			sawRewrite = true
		}
	}
	if !sawRewrite {
		t.Fatalf("anchor arm did not rewrite the selected meta slot %d — the load-bearing rewrite pwrite did not execute", selectedSlot)
	}
	if rec.syncs == 0 {
		t.Fatal("anchor arm did not fdatasync through the seam")
	}
}

// TestAnchorDivergentMetaFallsToRecoveryCommit pins the verified-
// identity anchor (durability.md §Anchoring — "byte-identity is
// VERIFIED, not assumed", the recovery instance): a checksum-valid
// meta carrying nonzero padding re-encodes to different bytes, so
// the anchor must NOT rewrite its slot (the changed-bytes hazard)
// and must NOT copy the same TxnID to the other slot either (an
// equal-TxnID pair is the commit-protocol violation that bricks
// meta selection) — it falls through to the recovery-commit
// publication: TxnID+1 to the other slot, after which selection
// still works and the divergent carrier is byte-identical.
func TestAnchorDivergentMetaFallsToRecoveryCommit(t *testing.T) {
	f, db, cleanup := initDB(t, false)
	defer cleanup()
	p := db.Pager
	ps := int(p.cfg.PageSize)

	// One self-durable commit past genesis, so the equal-TxnID
	// hazard regime (nonzero TxnIDs) is the one under test.
	prev, prevActive := db.Meta, db.ActiveMetaIdx
	p.BeginTx(TxParams{
		HighWaterMark: prev.HighWaterMark, MaxSize: prev.MaxSize,
		GrowStep: prev.GrowStep, MinSize: prev.MinSize, TxnID: 1,
		ReclamationBound: func() uint64 { return 0 },
	})
	id, err := p.AllocPage()
	if err != nil {
		t.Fatalf("AllocPage: %v", err)
	}
	if _, err := p.AllocSlab(id); err != nil {
		t.Fatalf("AllocSlab: %v", err)
	}
	res, err := p.Commit(CommitParams{NewTxnID: 1, Flags: prev.Flags, Sync: SyncBoth}, prev, prevActive)
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if !res.Meta.SelfDurable() || res.Meta.TxnID != 1 {
		t.Fatalf("fixture: TxnID=%d SelfDurable=%v, want 1/true", res.Meta.TxnID, res.Meta.SelfDurable())
	}
	slot := res.ActiveMetaIdx

	// Plant a nonzero padding byte, re-checksummed — the foreign
	// checksum-valid shape DecodeMeta ignores and EncodeMeta zeroes.
	page := make([]byte, ps)
	if _, err := f.ReadAt(page, int64(slot*ps)); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	page[metaOffPadding] = 0xA5
	binary.LittleEndian.PutUint64(page[MetaChecksumOffsetForTest():], ComputeMetaChecksum(page))
	if _, err := f.WriteAt(page, int64(slot*ps)); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	divergent := append([]byte(nil), page...)

	m, active, recovered, err := p.RecoverToDurable(f)
	if err != nil {
		t.Fatalf("RecoverToDurable: %v", err)
	}
	if !recovered || m.TxnID != 2 || !m.SelfDurable() {
		t.Fatalf("recovered=%v TxnID=%d SelfDurable=%v, want true/2/true (recovery-commit publication)", recovered, m.TxnID, m.SelfDurable())
	}
	if active != 1-slot {
		t.Fatalf("published slot = %d, want the OTHER slot %d", active, 1-slot)
	}
	// The divergent carrier is byte-identical to what we planted.
	if _, err := f.ReadAt(page, int64(slot*ps)); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if !bytes.Equal(page, divergent) {
		t.Fatal("the divergent slot was rewritten — the changed-bytes hazard the verification exists to avoid")
	}
	// Meta selection still works and picks the publication — the
	// equal-TxnID brick is the failure mode this pins against.
	latest, err := ReadLatestMeta(f, p.cfg.PageSize)
	if err != nil {
		t.Fatalf("ReadLatestMeta after divergent-carrier recovery: %v (meta selection bricked?)", err)
	}
	if latest.TxnID != 2 {
		t.Fatalf("latest TxnID = %d, want 2", latest.TxnID)
	}
}

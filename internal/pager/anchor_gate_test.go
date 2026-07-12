package pager

import (
	"bytes"
	"errors"
	"testing"

	"github.com/greatliontech/gmdb/internal/page"
)

// gateRecorder wraps FileOps recording meta writes + fsyncs so the
// tests can assert the tear-safe gate's byte-identical rewrite and its
// write→fsync ordering, and inject write/fsync failures.
type gateRecorder struct {
	inner  FileOps
	writes []struct {
		off  int64
		data []byte
	}
	syncs    int
	writeErr error
	syncErr  error
	syncSeen []int // len(writes) at each Fdatasync — ordering witness
}

func (r *gateRecorder) WriteAt(p []byte, off int64) (int, error) {
	if r.writeErr != nil {
		return 0, r.writeErr
	}
	buf := make([]byte, len(p))
	copy(buf, p)
	r.writes = append(r.writes, struct {
		off  int64
		data []byte
	}{off, buf})
	return r.inner.WriteAt(p, off)
}
func (r *gateRecorder) ReadAt(p []byte, off int64) (int, error) { return r.inner.ReadAt(p, off) }
func (r *gateRecorder) Truncate(size int64) error               { return r.inner.Truncate(size) }
func (r *gateRecorder) Fdatasync() error {
	r.syncs++
	r.syncSeen = append(r.syncSeen, len(r.writes))
	if r.syncErr != nil {
		return r.syncErr
	}
	return r.inner.Fdatasync()
}

// gateState is what gateFixture hands each test: the pager with a
// recorder installed, the adopted meta AS DECODED FROM DISK, the
// pre-gate on-disk bytes of the adopted slot, and the segment page.
type gateState struct {
	p       *Pager
	rec     *gateRecorder
	adopted Meta
	onDisk  []byte
	segPage uint64
}

// gateFixture builds a writer pager whose adopted active meta is
// SELF-DURABLE with a TRAILING persisted anchor (the pure-SyncDataOnly
// / checkpointed shape) and one pending RPL segment in the
// [anchored, durable) window — the exact state whose reclamation the
// trailing anchor withholds. The meta is ValidateMeta-clean and
// convention-consistent (RPLHeadTxnID names the head segment's TxnID),
// and adoption goes through DecodeMeta of the READ-BACK disk bytes —
// the production shape — so decode→re-encode drift is inside what the
// tests observe.
func gateFixture(t *testing.T) gateState {
	t.Helper()
	p, bm, f := setupWriter(t, 64)
	t.Cleanup(func() { p.Close(); f.Close() })
	first := bm.FirstDataPage()

	// One on-disk RPL segment at page first+5, TxnID 9, retiring
	// first+6 and first+7.
	segPage := first + 5
	segBuf := make([]byte, testPageSize)
	EncodeRPLSegment(segBuf, p.cfg, 9, 0, []uint64{first + 6, first + 7})
	if _, err := f.WriteAt(segBuf, int64(segPage)*int64(testPageSize)); err != nil {
		t.Fatalf("write segment: %v", err)
	}

	m := Meta{
		Magic:         page.Magic,
		Version:       page.FormatVersion,
		PageSize:      testPageSize,
		BitmapPages:   1,
		MaxSize:       64,
		MinSize:       8,
		GrowStep:      8,
		TxnID:         10,
		HighWaterMark: first + 10,
		RPLHeadPage:   segPage,
		RPLHeadTxnID:  9,
		RPLTailPage:   segPage,
		RPLEntryCount: 2,
	}
	m.Durable = m.LiveSubRecord()
	m.Durable.AnchoredTxnID = 9
	if err := ValidateMeta(m); err != nil {
		t.Fatalf("fixture meta not production-valid: %v", err)
	}
	metaBuf := make([]byte, testPageSize)
	EncodeMeta(metaBuf, &m)
	if _, err := f.WriteAt(metaBuf, 0); err != nil {
		t.Fatalf("write meta: %v", err)
	}

	onDisk := make([]byte, testPageSize)
	if _, err := f.ReadAt(onDisk, 0); err != nil {
		t.Fatalf("read back meta: %v", err)
	}
	if !VerifyMeta(onDisk) {
		t.Fatal("fixture meta fails checksum verification")
	}
	adopted := DecodeMeta(onDisk)

	rec := &gateRecorder{inner: p.fops}
	p.fops = rec
	p.noteAdoptedMeta(adopted, 0)
	// Mirror Resync's adoption: the persisted trailing anchor becomes
	// the in-process anchored epoch; the bound re-derives as
	// min(oldestReader, anchored) — no readers in this fixture.
	p.AdvanceAnchoredEpoch(adopted.Durable.AnchoredTxnID)
	p.refreshReclamationBound = func() uint64 { return p.AnchoredEpoch() }
	p.SetCommitState(adopted.HighWaterMark, adopted.MaxSize, 9) // bound = anchored = 9
	p.SetRPLChain([]RPLSegmentRef{{PageID: segPage, TxnID: 9, Count: 2}})
	return gateState{p: p, rec: rec, adopted: adopted, onDisk: onDisk, segPage: segPage}
}

// assertNoGateWrite fails if the recorder saw a meta-slot write.
func assertNoGateWrite(t *testing.T, rec *gateRecorder) {
	t.Helper()
	for _, w := range rec.writes {
		if w.off == 0 {
			t.Fatal("gate rewrote the meta slot in a state it must skip")
		}
	}
}

// The tear-safe anchor persist channel (durability.md §Anchoring): a
// handle that adopted a SELF-DURABLE meta with a trailing persisted
// anchor advances to the meta's own DurableTxnID only through its own
// byte-identical rewrite of the active slot plus a completed
// fdatasync, unblocking reclamation of the [anchored, durable) window
// — pre-fix, the window stayed withheld for the adopter (delayed
// reclamation; the checkpointing writer's in-process advance never
// reached peers).
func TestGateAnchorAdvanceUnblocksReclaim(t *testing.T) {
	s := gateFixture(t)

	n := s.p.ReclaimFreeSpace()
	if n == 0 {
		t.Fatal("reclaim freed nothing: the trailing anchor still withholds the [anchored, durable) window")
	}
	if got := s.p.AnchoredEpoch(); got != s.adopted.Durable.TxnID {
		t.Fatalf("anchored epoch = %d, want the adopted meta's own DurableTxnID %d", got, s.adopted.Durable.TxnID)
	}
	// The gate's rewrite is BYTE-IDENTICAL to the PRE-GATE ON-DISK
	// slot bytes (tear-safety: any torn mix of identical bytes is
	// identical — the sole durable carrier cannot be destroyed). The
	// baseline is the disk image captured by the fixture, NOT a
	// re-encode of the same struct, pinning the whole chain
	// disk → decode → cache → encode → == disk — a decode/encode
	// drift cannot hide behind both sides re-encoding identically.
	var gateWrite []byte
	for _, w := range s.rec.writes {
		if w.off == 0 && len(w.data) == testPageSize {
			gateWrite = w.data
			break
		}
	}
	if gateWrite == nil {
		t.Fatal("no meta rewrite recorded at the adopted slot")
	}
	if !bytes.Equal(gateWrite, s.onDisk) {
		t.Fatal("the gate's rewrite is not byte-identical to the pre-gate on-disk slot")
	}
	// Ordering: the fdatasync followed the rewrite.
	if s.rec.syncs == 0 || s.rec.syncSeen[0] < 1 {
		t.Fatalf("fdatasync did not follow the rewrite (syncs=%d, seen=%v)", s.rec.syncs, s.rec.syncSeen)
	}
	// The freed pages are back in the bitmap.
	if s.p.NumFreePages() < 2 {
		t.Fatalf("NumFreePages = %d, want the segment's 2 entries (+ the segment page) freed", s.p.NumFreePages())
	}
}

// A failed gate write or fsync leaves the anchor UNADVANCED and
// reclaims nothing — conservative (delayed reclamation, never a bound
// advance the disk did not witness).
func TestGateAnchorAdvanceFailureConservative(t *testing.T) {
	t.Run("fsync_failure", func(t *testing.T) {
		s := gateFixture(t)
		s.rec.syncErr = errors.New("injected fsync failure")
		if n := s.p.ReclaimFreeSpace(); n != 0 {
			t.Fatalf("reclaim freed %d pages under a failed gate fsync", n)
		}
		if got := s.p.AnchoredEpoch(); got != s.adopted.Durable.AnchoredTxnID {
			t.Fatalf("anchored epoch = %d advanced despite the failed fsync (want %d)", got, s.adopted.Durable.AnchoredTxnID)
		}
	})
	t.Run("write_failure", func(t *testing.T) {
		s := gateFixture(t)
		s.rec.writeErr = errors.New("injected write failure")
		if n := s.p.ReclaimFreeSpace(); n != 0 {
			t.Fatalf("reclaim freed %d pages under a failed gate write", n)
		}
		if got := s.p.AnchoredEpoch(); got != s.adopted.Durable.AnchoredTxnID {
			t.Fatalf("anchored epoch = %d advanced despite the failed write (want %d)", got, s.adopted.Durable.AnchoredTxnID)
		}
	})
}

// A divergence between the cached meta's re-encoding and the on-disk
// slot bytes must SKIP the gate (conservative): a changed-bytes
// rewrite of the sole durable carrier is exactly the hazard the
// channel exists to avoid, so byte-identity is verified, not assumed.
func TestGateAnchorAdvanceDivergenceSkips(t *testing.T) {
	s := gateFixture(t)
	// Diverge the on-disk slot AFTER adoption (one byte inside the
	// meta payload): the cached meta re-encodes to the ORIGINAL bytes,
	// so the gate's read-back comparison must refuse to write.
	if _, err := s.rec.inner.WriteAt([]byte{0xFF}, 100); err != nil {
		t.Fatalf("diverge disk: %v", err)
	}
	if n := s.p.ReclaimFreeSpace(); n != 0 {
		t.Fatalf("reclaimed %d despite a cache/disk divergence", n)
	}
	assertNoGateWrite(t, s.rec)
	if got := s.p.AnchoredEpoch(); got != s.adopted.Durable.AnchoredTxnID {
		t.Fatalf("anchored epoch advanced to %d despite a cache/disk divergence", got)
	}
}

// The gate never fires when it cannot help.
func TestGateAnchorAdvanceSkips(t *testing.T) {
	t.Run("reader_limited", func(t *testing.T) {
		s := gateFixture(t)
		// A reader pinned at 5: bound 5 < anchored 9 — advancing the
		// anchor moves nothing.
		s.p.refreshReclamationBound = func() uint64 { return min(5, s.p.AnchoredEpoch()) }
		s.p.SetCommitState(s.p.HighWaterMark(), 64, 5)
		if n := s.p.ReclaimFreeSpace(); n != 0 {
			t.Fatalf("reclaimed %d under a reader-limited bound", n)
		}
		assertNoGateWrite(t, s.rec)
	})
	t.Run("not_self_durable", func(t *testing.T) {
		s := gateFixture(t)
		// A meta whose Durable assertion is CARRIED FORWARD (TxnID 11,
		// Durable.TxnID 10 — the SyncLazy-commit shape), with the
		// anchored epoch (9) strictly below the carried assertion: the
		// channel's decided scope is SELF-DURABLE trailing metas only.
		// The carried-forward meta is written to DISK and adopted from
		// the read-back, so the gate's byte-identity check passes and
		// ONLY the self-durable scope check can protect here.
		m := s.adopted
		m.TxnID = 11
		m.Durable.TxnID = 10
		m.Durable.AnchoredTxnID = 9
		buf := make([]byte, testPageSize)
		EncodeMeta(buf, &m)
		if _, err := s.rec.inner.WriteAt(buf, 0); err != nil {
			t.Fatalf("write carried-forward meta: %v", err)
		}
		onDisk := make([]byte, testPageSize)
		if _, err := s.rec.inner.ReadAt(onDisk, 0); err != nil {
			t.Fatalf("read back: %v", err)
		}
		s.p.noteAdoptedMeta(DecodeMeta(onDisk), 0)
		if n := s.p.ReclaimFreeSpace(); n != 0 {
			t.Fatalf("reclaimed %d under a non-self-durable meta", n)
		}
		assertNoGateWrite(t, s.rec)
		if got := s.p.AnchoredEpoch(); got >= 10 {
			t.Fatalf("anchored epoch advanced to %d without the gate", got)
		}
	})
	t.Run("window_empty_chain", func(t *testing.T) {
		s := gateFixture(t)
		s.p.SetRPLChain(nil)
		if n := s.p.ReclaimFreeSpace(); n != 0 {
			t.Fatalf("reclaimed %d with an empty chain", n)
		}
		assertNoGateWrite(t, s.rec)
	})
	t.Run("window_oldest_at_durable", func(t *testing.T) {
		s := gateFixture(t)
		// The oldest pending segment sits AT the durable epoch: the
		// advance would not make it eligible (reclaim needs strictly
		// older) — no gain, no rewrite.
		s.p.SetRPLChain([]RPLSegmentRef{{PageID: s.segPage, TxnID: s.adopted.Durable.TxnID, Count: 2}})
		if n := s.p.ReclaimFreeSpace(); n != 0 {
			t.Fatalf("reclaimed %d with the oldest segment at the durable epoch", n)
		}
		assertNoGateWrite(t, s.rec)
	})
}

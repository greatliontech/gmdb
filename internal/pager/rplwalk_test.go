package pager

import (
	"fmt"
	"slices"
	"testing"

	"github.com/thegrumpylion/gmdb/internal/page"
)

// walkFixture is a synthetic on-disk RPL chain: a page-id → buffer map
// plus a free-bit map standing in for the allocation bitmap. Unknown
// page ids read as zeroed pages — the reclaimed-then-reused shape,
// classified as a footer failure when checksums are on (no valid
// footer) or a decode failure otherwise.
type walkFixture struct {
	cfg   page.Config
	pages map[uint64][]byte
	free  map[uint64]bool
	txns  map[uint64]uint64
}

func newWalkFixture(checksum bool) *walkFixture {
	return &walkFixture{
		cfg:   page.Config{PageSize: 4096, PageChecksum: checksum},
		pages: make(map[uint64][]byte),
		free:  make(map[uint64]bool),
	}
}

// segment writes a valid RPL segment page (with footer when checksums
// are on) at id.
func (f *walkFixture) segment(id, txnID, older uint64, entries ...uint64) {
	buf := make([]byte, f.cfg.PageSize)
	EncodeRPLSegment(buf, f.cfg, txnID, older, entries)
	if f.cfg.PageChecksum {
		page.WritePageFooter(buf, f.cfg.PageSize)
	}
	f.pages[id] = buf
	if f.txns == nil {
		f.txns = map[uint64]uint64{}
	}
	f.txns[id] = txnID
}

// garbage writes a page that carries a VALID footer (when checksums are
// on) but does not decode as a segment.
func (f *walkFixture) garbage(id uint64) {
	buf := make([]byte, f.cfg.PageSize)
	buf[0] = 0xEE // unknown page type
	if f.cfg.PageChecksum {
		page.WritePageFooter(buf, f.cfg.PageSize)
	}
	f.pages[id] = buf
}

// corrupt flips a payload byte after the footer was computed, so the
// footer no longer verifies.
func (f *walkFixture) corrupt(id uint64) {
	f.pages[id][RPLHeaderSize] ^= 0xFF
}

func (f *walkFixture) readPage(id uint64) []byte {
	if buf, ok := f.pages[id]; ok {
		return buf
	}
	return make([]byte, f.cfg.PageSize)
}

// chainWalk builds the RPLChainWalk with the fixture's oracle and
// standard bounds (LowBound 2, HighBound 1024) unless overridden.
func (f *walkFixture) chainWalk(head, tail, entryCount uint64) RPLChainWalk {
	return RPLChainWalk{
		ReadPage: f.readPage,
		Cfg:      f.cfg,
		Head:     head,
		Tail:     tail,
		// Honest projections record the head segment's own TxnID in
		// the meta (RPLHeadTxnID) — the authenticity ceiling. Garbage
		// or absent heads keep 0; those walks fail earlier anyway.
		HeadTxnID:  f.txns[head],
		EntryCount: entryCount,
		LowBound:   2,
		HighBound:  1024,
		IsFree:     func(id uint64) (bool, bool) { return f.free[id], true },
	}
}

// run walks and collects the visited segment page ids.
func run(t *testing.T, w RPLChainWalk) ([]uint64, RPLWalkStop, *RPLWalkError) {
	t.Helper()
	var visited []uint64
	stop, werr := w.Walk(func(id uint64, seg RPLSegment) bool {
		visited = append(visited, id)
		return true
	})
	return visited, stop, werr
}

func wantErr(t *testing.T, werr *RPLWalkError, kind RPLWalkErrKind, pageID uint64) {
	t.Helper()
	if werr == nil {
		t.Fatalf("Walk error = nil, want kind %d", kind)
	}
	if werr.Kind != kind {
		t.Fatalf("Walk error kind = %d (%v), want %d", werr.Kind, werr, kind)
	}
	if werr.PageID != pageID {
		t.Fatalf("Walk error page = %d, want %d", werr.PageID, pageID)
	}
}

func wantStop(t *testing.T, werr *RPLWalkError, stop RPLWalkStop, reason RPLWalkStopReason, pageID uint64) {
	t.Helper()
	if werr != nil {
		t.Fatalf("Walk error = %v, want stop reason %d", werr, reason)
	}
	if stop.Reason != reason || stop.PageID != pageID {
		t.Fatalf("Walk stop = {%d %d}, want {%d %d}", stop.Reason, stop.PageID, reason, pageID)
	}
}

// TestRPLChainWalkTraversal covers the non-error shapes of the shared
// chain-walk convention (free-space.md §Retired Page Log).
func TestRPLChainWalkTraversal(t *testing.T) {
	t.Run("empty chain", func(t *testing.T) {
		f := newWalkFixture(true)
		visited, stop, werr := run(t, f.chainWalk(0, 0, 0))
		wantStop(t, werr, stop, RPLWalkTailReached, 0)
		if len(visited) != 0 {
			t.Fatalf("visited %v, want none", visited)
		}
	})
	t.Run("single segment head==tail", func(t *testing.T) {
		f := newWalkFixture(true)
		f.segment(10, 5, 0, 100)
		visited, stop, werr := run(t, f.chainWalk(10, 10, 1))
		wantStop(t, werr, stop, RPLWalkTailReached, 10)
		if !slices.Equal(visited, []uint64{10}) {
			t.Fatalf("visited %v, want [10]", visited)
		}
	})
	t.Run("multi segment head to tail", func(t *testing.T) {
		f := newWalkFixture(true)
		f.segment(10, 5, 0, 100)  // original tail
		f.segment(11, 6, 10, 101) // middle
		f.segment(12, 7, 11, 102) // head
		visited, stop, werr := run(t, f.chainWalk(12, 10, 3))
		wantStop(t, werr, stop, RPLWalkTailReached, 10)
		if !slices.Equal(visited, []uint64{12, 11, 10}) {
			t.Fatalf("visited %v, want [12 11 10]", visited)
		}
	})
	t.Run("stops AT tail, never follows dangling OlderSegment", func(t *testing.T) {
		f := newWalkFixture(true)
		// Tail's OlderSegment dangles at 999 (reclaimed long ago, no
		// page there) — the walk must stop at the authoritative tail.
		f.segment(11, 6, 999, 101) // tail with dangling link
		f.segment(12, 7, 11, 102)  // head
		visited, stop, werr := run(t, f.chainWalk(12, 11, 2))
		wantStop(t, werr, stop, RPLWalkTailReached, 11)
		if !slices.Equal(visited, []uint64{12, 11}) {
			t.Fatalf("visited %v, want [12 11]", visited)
		}
	})
	t.Run("caller stop", func(t *testing.T) {
		f := newWalkFixture(true)
		f.segment(10, 5, 0, 100)
		f.segment(11, 6, 10, 101)
		w := f.chainWalk(11, 10, 2)
		stop, werr := w.Walk(func(id uint64, seg RPLSegment) bool { return false })
		wantStop(t, werr, stop, RPLWalkCallerStopped, 11)
	})
}

// TestRPLChainWalkBoundaries covers the sanctioned stale-tail
// truncation boundaries of recovery to a non-latest meta, including the
// head exemption (free-space.md §RPL, recovery to a non-latest meta).
func TestRPLChainWalkBoundaries(t *testing.T) {
	t.Run("reclaimed non-head truncates", func(t *testing.T) {
		f := newWalkFixture(true)
		f.segment(10, 5, 0, 100)
		f.segment(11, 6, 10, 101)
		f.segment(12, 7, 11, 102)
		f.free[11] = true // reclaimed from a stale tail
		visited, stop, werr := run(t, f.chainWalk(12, 10, 3))
		wantStop(t, werr, stop, RPLWalkReclaimedBoundary, 11)
		if !slices.Equal(visited, []uint64{12}) {
			t.Fatalf("visited %v, want [12]", visited)
		}
	})
	t.Run("head exempt from reclaimed boundary", func(t *testing.T) {
		// The head's free-bit is never consulted: the recovery target's
		// own newest segment is never legitimately reclaimed, so a set
		// bit on the head must not truncate the chain to nothing.
		f := newWalkFixture(true)
		f.segment(10, 5, 0, 100)
		f.segment(12, 7, 10, 102)
		f.free[12] = true // forged/impossible state — exemption applies
		visited, stop, werr := run(t, f.chainWalk(12, 10, 2))
		wantStop(t, werr, stop, RPLWalkTailReached, 10)
		if !slices.Equal(visited, []uint64{12, 10}) {
			t.Fatalf("visited %v, want [12 10]", visited)
		}
	})
	t.Run("non-head footer failure truncates", func(t *testing.T) {
		f := newWalkFixture(true)
		f.segment(10, 5, 0, 100)
		f.segment(11, 6, 10, 101)
		f.segment(12, 7, 11, 102)
		f.corrupt(11)
		visited, stop, werr := run(t, f.chainWalk(12, 10, 3))
		wantStop(t, werr, stop, RPLWalkFooterBoundary, 11)
		if !slices.Equal(visited, []uint64{12}) {
			t.Fatalf("visited %v, want [12]", visited)
		}
	})
	t.Run("non-head decode failure truncates", func(t *testing.T) {
		f := newWalkFixture(true)
		f.segment(11, 6, 999, 101) // 999: reused page, decodes as garbage
		f.segment(12, 7, 11, 102)
		// tail=999 simulates a stale tail pointer at a reused page: the
		// walk reaches 999 (valid-footer garbage) before the tail check
		// can stop it.
		f.garbage(999)
		visited, stop, werr := run(t, f.chainWalk(12, 999, 3))
		wantStop(t, werr, stop, RPLWalkDecodeBoundary, 999)
		if !slices.Equal(visited, []uint64{12, 11}) {
			t.Fatalf("visited %v, want [12 11]", visited)
		}
	})
	t.Run("oracle unavailable falls back to decode boundary", func(t *testing.T) {
		f := newWalkFixture(true)
		f.segment(10, 5, 0, 100)
		f.segment(12, 7, 10, 102)
		f.free[10] = true // would truncate — but the oracle is absent
		w := f.chainWalk(12, 10, 2)
		w.IsFree = func(uint64) (bool, bool) { return false, false }
		visited, stop, werr := run(t, w)
		wantStop(t, werr, stop, RPLWalkTailReached, 10)
		if !slices.Equal(visited, []uint64{12, 10}) {
			t.Fatalf("visited %v, want [12 10]", visited)
		}
	})
	t.Run("nil oracle reads as unavailable", func(t *testing.T) {
		f := newWalkFixture(true)
		f.segment(10, 5, 0, 100)
		f.segment(12, 7, 10, 102)
		w := f.chainWalk(12, 10, 2)
		w.IsFree = nil // must not panic; walk proceeds on footer/decode alone
		visited, stop, werr := run(t, w)
		wantStop(t, werr, stop, RPLWalkTailReached, 10)
		if !slices.Equal(visited, []uint64{12, 10}) {
			t.Fatalf("visited %v, want [12 10]", visited)
		}
	})
}

// TestRPLChainWalkErrors covers the hard structural-corruption
// classifications shared by the Open-time rebuild and Check.
func TestRPLChainWalkErrors(t *testing.T) {
	t.Run("tail missing", func(t *testing.T) {
		f := newWalkFixture(true)
		f.segment(12, 7, 0, 102)
		_, _, werr := run(t, f.chainWalk(12, 0, 1))
		wantErr(t, werr, RPLWalkErrTailMissing, 12)
	})
	t.Run("head out of range", func(t *testing.T) {
		f := newWalkFixture(true)
		w := f.chainWalk(2000, 10, 1) // HighBound 1024
		_, _, werr := run(t, w)
		wantErr(t, werr, RPLWalkErrSegmentOutOfRange, 2000)
	})
	t.Run("mid-chain link out of range", func(t *testing.T) {
		f := newWalkFixture(true)
		f.segment(12, 7, 2000, 102)
		_, _, werr := run(t, f.chainWalk(12, 10, 2))
		wantErr(t, werr, RPLWalkErrSegmentOutOfRange, 2000)
	})
	t.Run("link into meta/bitmap region", func(t *testing.T) {
		f := newWalkFixture(true)
		f.segment(12, 7, 1, 102) // OlderSegment points at a meta page
		_, _, werr := run(t, f.chainWalk(12, 10, 2))
		wantErr(t, werr, RPLWalkErrSegmentInMetaRegion, 1)
	})
	t.Run("cycle", func(t *testing.T) {
		f := newWalkFixture(true)
		f.segment(11, 6, 12, 101) // links back to head
		f.segment(12, 7, 11, 102)
		_, _, werr := run(t, f.chainWalk(12, 10, 8))
		wantErr(t, werr, RPLWalkErrChainCycle, 12)
	})
	t.Run("self-referential link is a cycle", func(t *testing.T) {
		f := newWalkFixture(true)
		f.segment(12, 7, 12, 102)
		_, _, werr := run(t, f.chainWalk(12, 10, 8))
		wantErr(t, werr, RPLWalkErrChainCycle, 12)
	})
	t.Run("chain longer than entry-count bound", func(t *testing.T) {
		f := newWalkFixture(true)
		f.segment(10, 5, 0, 100)
		f.segment(11, 6, 10, 101)
		f.segment(12, 7, 11, 102)
		// Forged EntryCount 0 → bound 1: the third segment trips it.
		_, _, werr := run(t, f.chainWalk(12, 10, 0))
		wantErr(t, werr, RPLWalkErrChainTooLong, 10)
	})
	t.Run("head checksum failure is hard", func(t *testing.T) {
		f := newWalkFixture(true)
		f.segment(10, 5, 0, 100)
		f.segment(12, 7, 10, 102)
		f.corrupt(12)
		_, _, werr := run(t, f.chainWalk(12, 10, 2))
		wantErr(t, werr, RPLWalkErrHeadChecksum, 12)
	})
	t.Run("head decode failure is hard", func(t *testing.T) {
		f := newWalkFixture(true)
		f.garbage(12)
		_, _, werr := run(t, f.chainWalk(12, 10, 1))
		wantErr(t, werr, RPLWalkErrHeadMalformed, 12)
	})
	t.Run("chain ends before tail", func(t *testing.T) {
		f := newWalkFixture(true)
		f.segment(11, 6, 0, 101) // OlderSegment 0 but tail says 10
		f.segment(12, 7, 11, 102)
		_, _, werr := run(t, f.chainWalk(12, 10, 2))
		wantErr(t, werr, RPLWalkErrEndedBeforeTail, 11)
	})
	t.Run("checksums off: corrupt payload is decode-classified", func(t *testing.T) {
		// Without footers a torn non-head segment can only surface via
		// decode failure — the FooterBoundary reason is unreachable.
		f := newWalkFixture(false)
		f.segment(11, 6, 10, 101)
		f.segment(12, 7, 11, 102)
		f.pages[11][0] = 0xEE // break the page type
		visited, stop, werr := run(t, f.chainWalk(12, 10, 3))
		wantStop(t, werr, stop, RPLWalkDecodeBoundary, 11)
		if !slices.Equal(visited, []uint64{12}) {
			t.Fatalf("visited %v, want [12]", visited)
		}
	})
}

// TestRPLChainWalkHeadOwnership pins the head-classification rule
// (free-space.md §Head classification requires the persisted head
// TxnID): the head is exempt from boundary treatment iff
// HeadTxnID >= ReclaimEpoch — from META state, never from decoding the
// head page. A carried-forward head (HeadTxnID < ReclaimEpoch) that
// was reclaimed and reused truncates like any stale tail; an owned
// head that fails is a hard error. This is the fix for the
// crash-mid-commit unopenable-database defect the old unconditional
// exemption carried.
func TestRPLChainWalkHeadOwnership(t *testing.T) {
	t.Run("carried head reclaimed-bit truncates to empty", func(t *testing.T) {
		f := newWalkFixture(true)
		f.segment(12, 7, 10, 102) // head, TxnID 7
		f.free[12] = true         // legitimately reclaimed carried head
		w := f.chainWalk(12, 10, 2)
		w.HeadTxnID = 7
		w.ReclaimEpoch = 9 // epoch 9 carried this older head forward
		visited, stop, werr := run(t, w)
		wantStop(t, werr, stop, RPLWalkReclaimedBoundary, 12)
		if len(visited) != 0 {
			t.Fatalf("visited %v, want none (whole chain behind the boundary)", visited)
		}
	})
	t.Run("carried head reused-page decode failure truncates", func(t *testing.T) {
		f := newWalkFixture(true)
		f.garbage(12) // reclaimed-and-reused with foreign valid-footer bytes
		w := f.chainWalk(12, 10, 2)
		w.HeadTxnID = 7
		w.ReclaimEpoch = 9
		visited, stop, werr := run(t, w)
		wantStop(t, werr, stop, RPLWalkDecodeBoundary, 12)
		if len(visited) != 0 {
			t.Fatalf("visited %v, want none", visited)
		}
	})
	t.Run("carried head torn-page footer failure truncates", func(t *testing.T) {
		f := newWalkFixture(true)
		f.segment(12, 7, 10, 102)
		f.corrupt(12)
		w := f.chainWalk(12, 10, 2)
		w.HeadTxnID = 7
		w.ReclaimEpoch = 9
		_, stop, werr := run(t, w)
		wantStop(t, werr, stop, RPLWalkFooterBoundary, 12)
	})
	t.Run("owned head failure stays a hard error", func(t *testing.T) {
		f := newWalkFixture(true)
		f.segment(10, 5, 0, 100)
		f.segment(12, 9, 10, 102)
		f.corrupt(12)
		w := f.chainWalk(12, 10, 2)
		w.HeadTxnID = 9
		w.ReclaimEpoch = 9 // the epoch's own head — never reclaimable
		_, _, werr := run(t, w)
		wantErr(t, werr, RPLWalkErrHeadChecksum, 12)
	})
	t.Run("live post-epoch head is exempt", func(t *testing.T) {
		f := newWalkFixture(true)
		f.segment(10, 5, 0, 100)
		f.segment(12, 11, 10, 102)
		f.corrupt(12)
		w := f.chainWalk(12, 10, 2)
		w.HeadTxnID = 11
		w.ReclaimEpoch = 9 // appended by an unfsynced later commit
		_, _, werr := run(t, w)
		wantErr(t, werr, RPLWalkErrHeadChecksum, 12)
	})
}

// A crashed commit can reuse a just-reclaimed chain page for its OWN
// new RPL segment; the adopted (older) meta still names the page, the
// impostor decodes validly, and its bitmap bit was re-cleared by the
// reuse — only TIME exposes it: chain TxnIDs are non-increasing
// head→tail and bounded by the head's. The walk must truncate at the
// impostor exactly like a reclaimed boundary (everything tailward was
// reclaimed by the same crashed pass), never accept it or error.
func TestRPLChainWalkTimeTraveledSegmentTruncates(t *testing.T) {
	for _, checksum := range []bool{true, false} {
		t.Run(fmt.Sprintf("checksum=%v", checksum), func(t *testing.T) {
			f := newWalkFixture(checksum)
			// Honest chain: head 30 (txn 9) → 20 (txn 8) → tail 10…
			// but page 10 was reclaimed and reused by crashed txn 11:
			// its content is now a NEWER segment.
			f.segment(30, 9, 20, 300)
			f.segment(20, 8, 10, 200)
			f.segment(10, 11, 0, 100) // impostor: txn 11 > predecessor's 8
			visited, stop, werr := run(t, f.chainWalk(30, 10, 3))
			wantStop(t, werr, stop, RPLWalkReclaimedBoundary, 10)
			if !slices.Equal(visited, []uint64{30, 20}) {
				t.Fatalf("visited %v, want [30 20]", visited)
			}

			// Two crash generations: the surviving impostor (txn 11)
			// now sits BELOW a newer head's ceiling (txn 15) — only
			// per-step non-increase exposes it.
			f2 := newWalkFixture(checksum)
			f2.segment(31, 15, 21, 310)
			f2.segment(21, 8, 11, 210)
			f2.segment(11, 11, 0, 110) // impostor from an earlier crash
			visited2, stop2, werr2 := run(t, f2.chainWalk(31, 11, 3))
			wantStop(t, werr2, stop2, RPLWalkReclaimedBoundary, 11)
			if !slices.Equal(visited2, []uint64{31, 21}) {
				t.Fatalf("gen-2 visited %v, want [31 21]", visited2)
			}
		})
	}
}

// Head-position impostor: the meta records the head's TxnID; on-disk
// content newer than that record is the same reuse class.
func TestRPLChainWalkTimeTraveledHeadTruncates(t *testing.T) {
	f := newWalkFixture(true)
	f.segment(30, 12, 20, 300) // content says txn 12…
	f.segment(20, 8, 0, 200)
	w := f.chainWalk(30, 20, 2)
	w.HeadTxnID = 9 // …but the adopted meta recorded 9
	_, stop, werr := run(t, w)
	wantStop(t, werr, stop, RPLWalkReclaimedBoundary, 30)
}

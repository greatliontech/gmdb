package pager

import (
	"fmt"

	"github.com/thegrumpylion/gmdb/internal/page"
)

// This file is the single implementation of the on-disk RPL chain-walk
// convention (free-space.md §Retired Page Log). Every consumer that
// walks the persisted chain RPLHeadPage → … → RPLTailPage — the
// Open-time in-memory rebuild and the root package's Check validator —
// goes through RPLChainWalk, so the boundary rules cannot drift between
// them:
//
//   - RPLTailPage is the authoritative terminator; the tail's on-disk
//     OlderSegment may dangle at a reclaimed page and is never followed.
//   - Recovery may select a non-latest meta whose tail is stale, so the
//     walk truncates at the first reclaimed segment: a page free in the
//     bitmap, or a non-head page that fails footer verification or no
//     longer decodes (reclaimed-then-reused).
//   - The head is exempt from the reclaimed-boundary treatment: the
//     recovery target's own newest segment is never legitimately
//     reclaimed, so a head that fails its footer or decode is a hard
//     error, not a stale boundary. This head-vs-non-head policy lives
//     only here.
//
// Runtime reclamation (reclaimRPL) consumes the in-memory segment list,
// not the on-disk links; it shares the per-segment read convention via
// readRPLSegment but not the chain walk.

// RPLWalkStopReason says why a walk ended without a hard error.
type RPLWalkStopReason int

const (
	// RPLWalkStopNone is the zero value, returned only alongside a
	// non-nil *RPLWalkError — check the error before the stop reason.
	RPLWalkStopNone RPLWalkStopReason = iota
	// RPLWalkTailReached: the walk consumed the chain through the
	// authoritative tail, or the chain was empty (head == 0).
	RPLWalkTailReached
	// RPLWalkReclaimedBoundary: a non-head segment page is free in the
	// allocation bitmap — the stale-tail shape of recovery to a
	// non-latest meta. The chain truncates here; the pages behind the
	// boundary are already free in the bitmap.
	RPLWalkReclaimedBoundary
	// RPLWalkFooterBoundary: a non-head segment failed checksum-footer
	// verification — reclaimed-then-reused with a torn or foreign
	// payload. Same truncation as RPLWalkReclaimedBoundary.
	RPLWalkFooterBoundary
	// RPLWalkDecodeBoundary: a non-head segment no longer decodes as an
	// RPL segment (reclaimed-then-reused). Same truncation.
	RPLWalkDecodeBoundary
	// RPLWalkCallerStopped: the visit callback returned false.
	RPLWalkCallerStopped
)

// RPLWalkStop describes how a walk ended without a hard error. PageID
// is the boundary segment page for the boundary reasons, the last
// visited page for RPLWalkTailReached/RPLWalkCallerStopped, and 0 for
// an empty chain.
type RPLWalkStop struct {
	Reason RPLWalkStopReason
	PageID uint64
}

// RPLWalkErrKind classifies a hard chain-walk failure — structural
// corruption of the chain itself, never the sanctioned stale-tail
// truncation.
type RPLWalkErrKind int

const (
	// RPLWalkErrTailMissing: head set but tail zero. buildNewMeta writes
	// head and tail both zero or both non-zero, so this is corrupt meta.
	RPLWalkErrTailMissing RPLWalkErrKind = iota
	// RPLWalkErrSegmentOutOfRange: a segment page id at or beyond
	// HighBound.
	RPLWalkErrSegmentOutOfRange
	// RPLWalkErrSegmentInMetaRegion: a segment page id below LowBound —
	// inside the meta/bitmap region, where no segment can live.
	// Reachable when a corrupt/forged meta's geometry (BitmapPages)
	// shifted the first data page above a persisted chain pointer.
	RPLWalkErrSegmentInMetaRegion
	// RPLWalkErrChainCycle: a segment page repeated on the walk (this
	// also catches a self-referential OlderSegment one step later).
	RPLWalkErrChainCycle
	// RPLWalkErrChainTooLong: more segments than EntryCount can account
	// for — a second line of defense against cycles/wild pointers.
	RPLWalkErrChainTooLong
	// RPLWalkErrHeadChecksum: the head segment failed checksum-footer
	// verification. The head is never legitimately reclaimed, so this is
	// corruption, not a stale boundary.
	RPLWalkErrHeadChecksum
	// RPLWalkErrHeadMalformed: the head segment failed to decode.
	RPLWalkErrHeadMalformed
	// RPLWalkErrEndedBeforeTail: OlderSegment == 0 before the walk
	// reached the authoritative tail.
	RPLWalkErrEndedBeforeTail
)

// RPLWalkError is a hard chain-walk failure. Callers map Kind to their
// own reporting (Open wraps ErrCorrupted/ErrBadPageChecksum; Check
// emits a CheckIssue per kind).
type RPLWalkError struct {
	Kind   RPLWalkErrKind
	PageID uint64 // segment page at fault
	Head   uint64
	Tail   uint64
	// Bound carries the violated limit: HighBound for
	// RPLWalkErrSegmentOutOfRange, LowBound for
	// RPLWalkErrSegmentInMetaRegion, the segment-count bound for
	// RPLWalkErrChainTooLong. Zero otherwise.
	Bound uint64
}

func (e *RPLWalkError) Error() string {
	switch e.Kind {
	case RPLWalkErrTailMissing:
		return fmt.Sprintf("RPL head %d set but tail is 0", e.Head)
	case RPLWalkErrSegmentOutOfRange:
		return fmt.Sprintf("RPL segment page %d beyond walk bound %d pages", e.PageID, e.Bound)
	case RPLWalkErrSegmentInMetaRegion:
		return fmt.Sprintf("RPL segment page %d inside the meta/bitmap region (firstDataPage=%d)", e.PageID, e.Bound)
	case RPLWalkErrChainCycle:
		return fmt.Sprintf("RPL chain cycle at page %d", e.PageID)
	case RPLWalkErrChainTooLong:
		return fmt.Sprintf("RPL chain exceeds bound %d (likely cycle)", e.Bound)
	case RPLWalkErrHeadChecksum:
		return fmt.Sprintf("RPL head segment at page %d fails checksum", e.PageID)
	case RPLWalkErrHeadMalformed:
		return fmt.Sprintf("RPL head segment at page %d malformed", e.PageID)
	case RPLWalkErrEndedBeforeTail:
		return fmt.Sprintf("RPL chain from head %d ended before tail %d", e.Head, e.Tail)
	}
	return fmt.Sprintf("RPL chain walk error kind %d at page %d", e.Kind, e.PageID)
}

// RPLChainWalk parameterizes one on-disk chain walk. The reader and
// bounds are explicit (rather than Pager fields) so the Open-time
// rebuild can walk against not-yet-installed state (attachState stays
// atomic) and the root package's Check can walk its own snapshot.
type RPLChainWalk struct {
	// ReadPage returns the full page buffer for a page id. Ids are
	// bounds-checked against [LowBound, HighBound) before ReadPage is
	// called, so a raw accessor that panics out of range is safe here.
	ReadPage func(uint64) []byte
	Cfg      page.Config
	// Head, Tail, EntryCount come from the meta being walked
	// (RPLHeadPage, RPLTailPage, RPLEntryCount).
	Head       uint64
	Tail       uint64
	EntryCount uint64
	// LowBound is the first data page (meta/bitmap region ends there);
	// HighBound is one past the last walkable page id. HighBound must be
	// trustworthy against a forged meta — the file-resident extent
	// capped by MaxSize (optionally tightened by a clamped
	// HighWaterMark), never a raw meta field that can point past the
	// mmap reservation.
	LowBound  uint64
	HighBound uint64
	// IsFree is the allocation-bitmap oracle for the reclaimed-segment
	// boundary; the second result reports oracle availability (Check
	// falls back to the footer/decode boundary alone without one). May
	// be nil, which reads as "oracle unavailable".
	IsFree func(uint64) (bool, bool)
}

// Walk traverses the chain head → tail, calling visit for each segment
// that passes the shared read convention (footer verification before
// decode, per checksums.md §Verification — ReadPage is expected to be a
// raw, non-verifying accessor). Returns how the walk stopped, or a hard
// error; exactly one of the two is meaningful (stop is zero-valued on
// error).
func (w RPLChainWalk) Walk(visit func(id uint64, seg page.RPLSegment) bool) (RPLWalkStop, *RPLWalkError) {
	if w.Head == 0 {
		return RPLWalkStop{Reason: RPLWalkTailReached}, nil
	}
	if w.Tail == 0 {
		return RPLWalkStop{}, &RPLWalkError{Kind: RPLWalkErrTailMissing, PageID: w.Head, Head: w.Head}
	}
	// Upper bound on segment count: every segment holds ≥1 entry, so a
	// valid chain has at most EntryCount segments. The +1 slack covers
	// the trivial empty-chain case (EntryCount==0 with a stale head from
	// a partial pwrite would be one excess segment); cycles are caught
	// by the visited set, so the count bound is a belt-and-suspenders
	// second line of defense. The map-capacity hint is capped: EntryCount
	// is read from a possibly-forged meta and must not size an
	// allocation.
	maxSegs := w.EntryCount + 1
	visited := make(map[uint64]struct{}, min(maxSegs, 1024))
	var segs uint64
	id := w.Head
	for {
		if id >= w.HighBound {
			return RPLWalkStop{}, &RPLWalkError{Kind: RPLWalkErrSegmentOutOfRange, PageID: id, Head: w.Head, Tail: w.Tail, Bound: w.HighBound}
		}
		if id < w.LowBound {
			return RPLWalkStop{}, &RPLWalkError{Kind: RPLWalkErrSegmentInMetaRegion, PageID: id, Head: w.Head, Tail: w.Tail, Bound: w.LowBound}
		}
		if _, seen := visited[id]; seen {
			return RPLWalkStop{}, &RPLWalkError{Kind: RPLWalkErrChainCycle, PageID: id, Head: w.Head, Tail: w.Tail}
		}
		if segs > maxSegs {
			return RPLWalkStop{}, &RPLWalkError{Kind: RPLWalkErrChainTooLong, PageID: id, Head: w.Head, Tail: w.Tail, Bound: maxSegs}
		}
		// Reclaimed-segment boundary — never the head (see the file
		// comment: the head-vs-non-head policy is pinned here).
		if id != w.Head && w.IsFree != nil {
			if free, ok := w.IsFree(id); ok && free {
				return RPLWalkStop{Reason: RPLWalkReclaimedBoundary, PageID: id}, nil
			}
		}
		visited[id] = struct{}{}
		seg, footerOK, ok := readRPLSegment(w.ReadPage, w.Cfg, id)
		if !footerOK {
			if id == w.Head {
				return RPLWalkStop{}, &RPLWalkError{Kind: RPLWalkErrHeadChecksum, PageID: id, Head: w.Head, Tail: w.Tail}
			}
			return RPLWalkStop{Reason: RPLWalkFooterBoundary, PageID: id}, nil
		}
		if !ok {
			if id == w.Head {
				return RPLWalkStop{}, &RPLWalkError{Kind: RPLWalkErrHeadMalformed, PageID: id, Head: w.Head, Tail: w.Tail}
			}
			return RPLWalkStop{Reason: RPLWalkDecodeBoundary, PageID: id}, nil
		}
		if !visit(id, seg) {
			return RPLWalkStop{Reason: RPLWalkCallerStopped, PageID: id}, nil
		}
		segs++
		if id == w.Tail {
			// Authoritative tail — never follow its (possibly dangling)
			// OlderSegment.
			return RPLWalkStop{Reason: RPLWalkTailReached, PageID: id}, nil
		}
		next := seg.OlderSegment
		if next == 0 {
			return RPLWalkStop{}, &RPLWalkError{Kind: RPLWalkErrEndedBeforeTail, PageID: id, Head: w.Head, Tail: w.Tail}
		}
		id = next
	}
}

// readRPLSegment reads segment page id via readPage and applies the
// shared per-segment read convention: checksum-footer verification
// (when PageChecksum is on) BEFORE decode, per checksums.md
// §Verification — raw accessors do not verify. footerOK reports the
// footer check; ok reports the decode (meaningful only when footerOK).
// The caller bounds id before calling: readPage may be a raw accessor
// that panics out of range.
func readRPLSegment(readPage func(uint64) []byte, cfg page.Config, id uint64) (seg page.RPLSegment, footerOK, ok bool) {
	buf := readPage(id)
	if cfg.PageChecksum && !page.VerifyPageFooter(buf, cfg.PageSize) {
		return page.RPLSegment{}, false, false
	}
	seg, ok = page.DecodeRPLSegment(buf, cfg)
	return seg, true, ok
}

package bitmap

import (
	"encoding/binary"
	"fmt"
	"maps"
	"math/bits"
	"slices"
)

var le = binary.LittleEndian

// Bitmap is the in-memory allocation bitmap. A **set** bit means the page
// is free and safe to allocate; a **clear** bit means the page is in use,
// retired (pending RPL reclamation), or one of the permanently-clear meta
// or bitmap pages.
//
// Backed by a writable []byte (detail level) plus an in-memory []uint64
// summary, one summary bit per detail uint64 word. The detail is what the
// pager pwrites to the on-disk bitmap region at commit; the summary is a
// memory-only acceleration structure rebuilt at Open and maintained
// incrementally.
//
// Bitmap is not safe for concurrent use. The pager serialises access via
// the writer lock (the bitmap is read read-only during a read transaction
// — but read transactions don't touch the bitmap; only the writer does).
type Bitmap struct {
	detail        []byte
	summary       []uint64
	pageSize      uint32
	bitmapPages   uint32
	firstDataPage uint64
	totalPages    uint64
	numFree       uint64
	hint          uint64
	dirty         map[uint32]struct{}

	// undoLog records every bit flip performed while at least one
	// Snapshot is open. Each entry's wasSet is the bit value BEFORE the
	// flip, so re-applying it (undoFlip) reverts the mutation. The log
	// is shared across all open Snapshots — strict-LIFO callers replay
	// from the topmost snapshot's logPos, then truncate. Truncated to
	// length 0 by Discard once no Snapshot remains open, so memory does
	// not survive across transactions.
	//
	// Encodes the clause-explicit cost invariant in transactions.md
	// §Nested Transactions ("Cost is proportional to pages modified at
	// that level, not total database size"): memory is O(flips since
	// the outermost open snapshot began), not O(MaxSize/PageSize/8) as
	// it was when Snapshot cloned the whole detail+summary.
	undoLog []bitmapFlip

	// openSnapshots tracks every Snapshot returned by Snapshot() that
	// has not yet been Restored or Discarded. Slice order is begin-
	// order (outermost first); strict-LIFO contract per the pager's
	// Nesting per transactions.md §Why this is cheap. recordFlip
	// is a no-op when this is empty — no Restore can replay an entry
	// no Snapshot will reference.
	openSnapshots []*Snapshot
}

// bitmapFlip is one entry of the per-bitmap undo log: the page that was
// flipped and the bit value it held BEFORE the flip. undoFlip re-applies
// that prior value, reverting the mutation idempotently (a Set on an
// already-set bit / Clear on an already-clear bit is a no-op).
type bitmapFlip struct {
	page   uint64
	wasSet bool
}

// New constructs a Bitmap from an existing detail byte slice. The slice is
// adopted as the bitmap's writable backing store — callers must not reuse
// it after handing it over.
//
//   - pageSize: a power-of-two page size in [4 KB, 64 KB]. Panics on
//     anything else; the spec invariant rules out the looser cases.
//   - bitmapPages: count of bitmap pages (file-layout invariant). detail
//     must be exactly bitmapPages*pageSize bytes long.
//   - totalPages: number of pages tracked = MaxSize / PageSize. Bits at
//     indices >= totalPages are tail-of-bitmap padding and must remain
//     clear.
//
// New is defense-in-depth on the invariants from `free-space.md
// §Allocation Bitmap`: bits below `firstDataPage = 2 + bitmapPages` and
// bits at-or-above `totalPages` are forcibly cleared on intake. Set()
// and Clear() panic on the same regions, but a corrupt or hostile detail
// buffer must not get to feed false free-bits to FindFirst / Recount; the
// constructor closes that hole.
func New(detail []byte, pageSize, bitmapPages uint32, totalPages uint64) *Bitmap {
	if pageSize < 4096 || pageSize > 65536 || pageSize&(pageSize-1) != 0 {
		panic(fmt.Sprintf("bitmap: invalid pageSize %d (must be a power of two in [4096, 65536])", pageSize))
	}
	want := uint64(bitmapPages) * uint64(pageSize)
	if uint64(len(detail)) != want {
		panic(fmt.Sprintf("bitmap: detail length %d != bitmapPages*pageSize %d", len(detail), want))
	}
	if totalPages > uint64(len(detail))*8 {
		panic(fmt.Sprintf("bitmap: totalPages %d exceeds bitmap capacity %d", totalPages, uint64(len(detail))*8))
	}
	firstDataPage := uint64(2) + uint64(bitmapPages)
	b := &Bitmap{
		detail:        detail,
		pageSize:      pageSize,
		bitmapPages:   bitmapPages,
		firstDataPage: firstDataPage,
		totalPages:    totalPages,
		hint:          firstDataPage,
		dirty:         make(map[uint32]struct{}),
	}
	b.maskInvariantRegions()
	b.rebuildSummary()
	return b
}

// maskInvariantRegions forcibly clears bits in the permanently-clear
// region (pages 0..firstDataPage-1) and in the tail-past-totalPages region.
// Called once at construction; no dirty marks (the on-disk image is
// authoritative for those regions and we are merely enforcing the spec
// invariant in memory).
func (b *Bitmap) maskInvariantRegions() {
	// Clear pages 0..firstDataPage-1.
	for p := uint64(0); p < b.firstDataPage; p++ {
		byteIdx := p >> 3
		bitIdx := uint(p & 7)
		b.detail[byteIdx] &^= 1 << bitIdx
	}
	// Clear bits at indices >= totalPages within the detail.
	for p := b.totalPages; p < uint64(len(b.detail))*8; p++ {
		byteIdx := p >> 3
		bitIdx := uint(p & 7)
		b.detail[byteIdx] &^= 1 << bitIdx
	}
}

// FirstDataPage returns the lowest page ID that may carry a set bit: the
// first page after the meta + bitmap region.
func (b *Bitmap) FirstDataPage() uint64 { return b.firstDataPage }

// TotalPages returns the configured detail capacity in pages.
func (b *Bitmap) TotalPages() uint64 { return b.totalPages }

// NumFree returns the count of set bits across the valid region of the
// detail level. Maintained incrementally; verified by Recount.
func (b *Bitmap) NumFree() uint64 { return b.numFree }

// Hint returns the LIFO allocation hint.
func (b *Bitmap) Hint() uint64 { return b.hint }

// SetHint stores a new LIFO allocation hint. The hint is clamped to
// [firstDataPage, totalPages); out-of-range values silently reset to
// firstDataPage. This is friendlier than panicking when the hint becomes
// stale (e.g., the most recent free was tail-refunded and is now
// out-of-range) and matches the spec's positioning of the hint as a
// best-effort locality optimisation, not a correctness signal.
func (b *Bitmap) SetHint(p uint64) {
	if p < b.firstDataPage || p >= b.totalPages {
		p = b.firstDataPage
	}
	b.hint = p
}

// IsSet reports whether the bit for page is set (free). Returns false for
// out-of-range page indices.
func (b *Bitmap) IsSet(page uint64) bool {
	if page >= b.totalPages {
		return false
	}
	byteIdx := page >> 3
	bitIdx := uint(page & 7)
	return b.detail[byteIdx]&(1<<bitIdx) != 0
}

// Set marks page as free (bit = 1). Panics on pages in the permanently-
// clear region (meta + bitmap region) per the spec invariant — that case
// is a programming error in the caller, not a recoverable runtime
// condition.
func (b *Bitmap) Set(page uint64) {
	b.checkAllocatable(page, "Set")
	byteIdx := page >> 3
	bitIdx := uint(page & 7)
	mask := byte(1) << bitIdx
	if b.detail[byteIdx]&mask != 0 {
		return // already set
	}
	// Record the pre-flip bit value (clear) before mutating, so a future
	// Restore can re-apply it. No-op when no Snapshot is open.
	b.recordFlip(page, false)
	b.detail[byteIdx] |= mask
	b.numFree++
	b.markSummary(page)
	b.markDirty(page)
}

// Clear marks page as allocated (bit = 0). Panics on pages in the
// permanently-clear region.
func (b *Bitmap) Clear(page uint64) {
	b.checkAllocatable(page, "Clear")
	byteIdx := page >> 3
	bitIdx := uint(page & 7)
	mask := byte(1) << bitIdx
	if b.detail[byteIdx]&mask == 0 {
		return // already clear
	}
	// Record the pre-flip bit value (set) before mutating, so a future
	// Restore can re-apply it. No-op when no Snapshot is open.
	b.recordFlip(page, true)
	b.detail[byteIdx] &^= mask
	b.numFree--
	b.unmarkSummaryIfWordZero(page)
	b.markDirty(page)
}

// FindFirst returns the lowest free page id at or after b.hint, wrapping
// once around to the start of the data region. ok=false means no free
// page exists.
func (b *Bitmap) FindFirst() (uint64, bool) {
	if b.numFree == 0 {
		return 0, false
	}
	if p, ok := b.scanForward(b.hint, b.totalPages); ok {
		return p, true
	}
	if p, ok := b.scanForward(b.firstDataPage, b.hint); ok {
		return p, true
	}
	return 0, false
}

// FindContiguous returns the starting page id of the lowest run of n
// consecutive free pages at or after b.hint, wrapping once around. n must
// be >= 1. Returns ok=false if no such run exists.
//
// Implemented per free-space.md §Bitmap operations: word-level scan with
// math/bits.TrailingZeros64 to find runs within words and a carry-forward
// run length across word boundaries. O(scanned words).
func (b *Bitmap) FindContiguous(n int) (uint64, bool) {
	if n <= 0 {
		return 0, false
	}
	if n == 1 {
		return b.FindFirst()
	}
	if uint64(n) > b.numFree {
		return 0, false
	}
	if p, ok := b.runForward(b.hint, b.totalPages, n); ok {
		return p, true
	}
	// Wrap pass: searching [firstDataPage, hint+n-1) lets us find runs
	// whose start position is in [firstDataPage, hint) — including runs
	// that straddle the wrap point in the sense that their starting
	// position is below hint but they extend up to (hint+n-1).
	end := min(b.hint+uint64(n)-1, b.totalPages)
	if p, ok := b.runForward(b.firstDataPage, end, n); ok {
		return p, true
	}
	return 0, false
}

// DirtyPages returns the sorted list of bitmap-page indices (0-based
// relative to the bitmap region; the on-disk page id is 2 + idx) that
// have been modified since the last ClearDirty call. Returns nil when
// nothing is dirty (saves an allocation in the commit hot path).
func (b *Bitmap) DirtyPages() []uint32 {
	if len(b.dirty) == 0 {
		return nil
	}
	out := make([]uint32, 0, len(b.dirty))
	for i := range b.dirty {
		out = append(out, i)
	}
	slices.Sort(out)
	return out
}

// PageBytes returns the detail-level bytes backing the bitmap page at the
// given index (0..bitmapPages-1). The returned slice aliases the bitmap's
// storage and stays valid until the Bitmap is dropped; callers must not
// retain it across a New() rebuild.
func (b *Bitmap) PageBytes(idx uint32) []byte {
	if idx >= b.bitmapPages {
		panic(fmt.Sprintf("bitmap: PageBytes(%d) out of range [0, %d)", idx, b.bitmapPages))
	}
	off := int(idx) * int(b.pageSize)
	return b.detail[off : off+int(b.pageSize)]
}

// ClearDirty resets the dirty set. Called by the pager after a successful
// commit's bitmap-page pwrites.
func (b *Bitmap) ClearDirty() {
	if len(b.dirty) == 0 {
		return
	}
	b.dirty = make(map[uint32]struct{})
}

// Snapshot is a marker into the Bitmap's per-tx undo log that the pager
// uses to roll back tx-scoped mutations (AllocPage → bitmap.Clear,
// reclaimRPL → bitmap.Set, TailRefund → bitmap.Clear, loose→pendingFrees
// → bitmap.Set) when a commit aborts or a child savepoint rolls back.
//
// Snapshot captures (hint, numFree, dirty) by clone and records the
// undo-log position at which it began; subsequent Set/Clear append an
// undo entry for each real bit flip while at least one Snapshot is
// open. Restore replays the log in reverse from the Snapshot's logPos
// and reinstalls the captured scalars + dirty set; Discard releases
// the Snapshot without replaying.
//
// Cost contract (transactions.md §Nested Transactions "Cost is
// proportional to pages modified since the outermost open savepoint,
// plus O(bitmap-pages currently dirty) ..."): per-Snapshot memory is
// O(bits flipped while this Snapshot is open) + O(distinct bitmap
// pages dirty at Snapshot time). The dirty clone is bounded by
// bitmapPages (≤ 2048 at 256 GB / 4 KB) and is the only fixed-shape
// cost; the flip log is bounded by actual mutation count, not by
// MaxSize.
//
// Strict-LIFO contract: Snapshots returned by Snapshot() must be passed
// to Restore or Discard in reverse begin-order. Restoring a non-top
// Snapshot also invalidates every Snapshot opened after it (their
// logPos would index into a now-truncated log); the implementation
// pops them defensively, but a caller that later passes one of those
// to Restore or Discard hits the "Snapshot is not open" panic. The
// pager's BeginTx/BeginSavepoint pairing already enforces LIFO; the
// parent-freeze rule (Commit frozen; Rollback cascades deepest-
// first) means children always resolve
// before their parent commits or rolls back.
type Snapshot struct {
	bitmap  *Bitmap
	logPos  int
	hint    uint64
	numFree uint64
	dirty   map[uint32]struct{}
}

// Snapshot opens a new rollback marker at the current Bitmap state.
// Set/Clear after this point append undo entries to b.undoLog until
// the returned *Snapshot is passed to Restore (revert) or Discard
// (release without revert). See the Snapshot type godoc for the cost
// contract.
func (b *Bitmap) Snapshot() *Snapshot {
	s := &Snapshot{
		bitmap:  b,
		logPos:  len(b.undoLog),
		hint:    b.hint,
		numFree: b.numFree,
		dirty:   maps.Clone(b.dirty),
	}
	b.openSnapshots = append(b.openSnapshots, s)
	return s
}

// Restore reverts the Bitmap to the state at s's Snapshot() call.
// After Restore, b.detail, b.summary, b.numFree, b.hint, and b.dirty
// are byte- and element-identical to that state. The Bitmap struct is
// otherwise unchanged (pageSize, bitmapPages, firstDataPage,
// totalPages remain — they are configuration, not state).
//
// Replays the undo-log entries appended since s's begin in reverse
// order, applying each flip's pre-flip bit value directly to the
// detail; summary is updated incrementally so its post-Restore state
// matches the rebuilt detail without an O(detail-size) Recount.
// numFree and hint come from s's captured scalars; dirty is reinstalled
// from s's captured clone (a clone, not aliased — s is consumed).
//
// Strict LIFO: if s is not the topmost open Snapshot, every Snapshot
// opened after s is also popped (their logPos would index past the
// truncated log). Passing a non-open Snapshot (already restored,
// discarded, or never opened on this Bitmap) panics.
func (b *Bitmap) Restore(s *Snapshot) {
	idx := b.indexOfSnapshot(s)
	if idx < 0 {
		panic("bitmap: Restore called on a Snapshot that is not open")
	}
	// Replay log[s.logPos..end] in reverse. Each entry's wasSet is the
	// bit value BEFORE the flip; re-applying it inverts the original
	// mutation. undoFlip handles the detail bit and the summary update;
	// numFree and dirty are restored wholesale below from s's captures.
	for i := len(b.undoLog) - 1; i >= s.logPos; i-- {
		e := b.undoLog[i]
		b.undoFlip(e.page, e.wasSet)
	}
	b.undoLog = b.undoLog[:s.logPos]
	b.hint = s.hint
	b.numFree = s.numFree
	// Adopt s.dirty: s is consumed by Restore (popped from openSnapshots
	// below), so no aliasing concern. Saves the clone.
	b.dirty = s.dirty
	// Pop s and every later (inner) Snapshot. Strict-LIFO callers Restore
	// innermost first, so idx is typically len(openSnapshots)-1 here;
	// the cascade defends against callers that skip levels and prevents
	// a later Restore from indexing past the now-truncated log.
	b.openSnapshots = b.openSnapshots[:idx]
}

// Discard releases s without replaying — used by ReleaseSavepoint
// (child commit) and the top-level Commit-success path. s's undo-log
// entries remain in b.undoLog and contribute to any still-open outer
// Snapshot's potential Restore; if s was the last open Snapshot,
// b.undoLog is truncated to length 0 so memory doesn't survive across
// transactions.
//
// Passing a non-open Snapshot panics (mirrors Restore).
func (b *Bitmap) Discard(s *Snapshot) {
	idx := b.indexOfSnapshot(s)
	if idx < 0 {
		panic("bitmap: Discard called on a Snapshot that is not open")
	}
	b.openSnapshots = slices.Delete(b.openSnapshots, idx, idx+1)
	if len(b.openSnapshots) == 0 {
		// No Snapshot left to replay the log; free the backing array so
		// it doesn't survive the transaction boundary. A new Snapshot()
		// will start with logPos=0 against the cleared slice.
		b.undoLog = b.undoLog[:0]
	}
}

// indexOfSnapshot returns the index of s in b.openSnapshots, or -1 if
// not present. Pointer-identity lookup; Snapshots are not value-
// comparable (they hold maps).
func (b *Bitmap) indexOfSnapshot(s *Snapshot) int {
	for i, os := range b.openSnapshots {
		if os == s {
			return i
		}
	}
	return -1
}

// recordFlip appends one undo-log entry. wasSet is the bit value
// BEFORE the flip. Skips the append when no Snapshot is open: there
// is no caller that could ever Restore through this entry, so the log
// would only grow.
func (b *Bitmap) recordFlip(page uint64, wasSet bool) {
	if len(b.openSnapshots) == 0 {
		return
	}
	b.undoLog = append(b.undoLog, bitmapFlip{page: page, wasSet: wasSet})
}

// undoFlip re-applies the pre-flip bit value during Restore replay.
// Directly toggles the detail bit and updates the summary; does NOT
// touch numFree or dirty (Restore restores those from the Snapshot's
// captured scalar / map). Idempotent: re-setting an already-set bit
// or re-clearing an already-clear bit is a no-op, which makes the
// replay safe for the cascade-pop case where outer-Restore re-applies
// entries already reverted by an inner Restore.
func (b *Bitmap) undoFlip(page uint64, wasSet bool) {
	byteIdx := page >> 3
	bitIdx := uint(page & 7)
	mask := byte(1) << bitIdx
	if wasSet {
		b.detail[byteIdx] |= mask
		b.markSummary(page)
	} else {
		b.detail[byteIdx] &^= mask
		b.unmarkSummaryIfWordZero(page)
	}
}

// Recount recomputes NumFree from the detail via popcnt, scoped to the
// valid region [firstDataPage, totalPages). Used by Check() and after
// externally-driven rebuilds. The maintained NumFree is updated to the
// recomputed value.
func (b *Bitmap) Recount() uint64 {
	var n uint64
	// Walk only the valid word range. The permanently-clear region and
	// the tail-past-totalPages region are masked at intake; we mask
	// them again here defensively in case future call sites push raw
	// detail bytes in without going through New.
	numWords := len(b.detail) / 8
	for i := range numWords {
		w := readDetailWord(b.detail, i)
		// Mask off bits below firstDataPage in the first word(s).
		if uint64(i*64) < b.firstDataPage {
			lo := b.firstDataPage - uint64(i*64)
			if lo >= 64 {
				continue
			}
			w &^= (uint64(1) << uint(lo)) - 1
		}
		// Mask off bits at-or-above totalPages in the last word(s).
		if uint64(i*64+64) > b.totalPages {
			hi := b.totalPages - uint64(i*64)
			if hi >= 64 {
				// All bits in this word are in range; no mask.
			} else if hi == 0 {
				break
			} else {
				w &= (uint64(1) << uint(hi)) - 1
			}
		}
		n += uint64(bits.OnesCount64(w))
	}
	b.numFree = n
	return n
}

// internals ---------------------------------------------------------------

func (b *Bitmap) checkAllocatable(page uint64, op string) {
	if page >= b.totalPages {
		panic(fmt.Sprintf("bitmap: %s(%d): page out of range (totalPages=%d)", op, page, b.totalPages))
	}
	if page < b.firstDataPage {
		panic(fmt.Sprintf("bitmap: %s(%d): page in permanently-clear region (firstDataPage=%d)", op, page, b.firstDataPage))
	}
}

func (b *Bitmap) markDirty(page uint64) {
	bytesPerPage := uint64(b.pageSize)
	bitsPerPage := bytesPerPage * 8
	idx := uint32(page / bitsPerPage)
	b.dirty[idx] = struct{}{}
}

func (b *Bitmap) rebuildSummary() {
	numWords := len(b.detail) / 8
	summaryWords := (numWords + 63) / 64
	b.summary = make([]uint64, summaryWords)
	var free uint64
	for i := range numWords {
		w := readDetailWord(b.detail, i)
		if w == 0 {
			continue
		}
		b.summary[i>>6] |= uint64(1) << uint(i&63)
		free += uint64(bits.OnesCount64(w))
	}
	// The post-intake invariant is that bits outside [firstDataPage,
	// totalPages) are clear (maskInvariantRegions cleared them), so the
	// popcount is already in-range.
	b.numFree = free
}

func (b *Bitmap) markSummary(page uint64) {
	wordIdx := page >> 6
	b.summary[wordIdx>>6] |= uint64(1) << uint(wordIdx&63)
}

func (b *Bitmap) unmarkSummaryIfWordZero(page uint64) {
	wordIdx := page >> 6
	w := readDetailWord(b.detail, int(wordIdx))
	if w == 0 {
		b.summary[wordIdx>>6] &^= uint64(1) << uint(wordIdx&63)
	}
}

// scanForward returns the first set bit in detail covering [from, to),
// using the summary to skip empty regions. Pages below b.firstDataPage are
// skipped; pages at-or-above b.totalPages are skipped.
func (b *Bitmap) scanForward(from, to uint64) (uint64, bool) {
	from = max(from, b.firstDataPage)
	to = min(to, b.totalPages)
	if from >= to {
		return 0, false
	}
	wordFrom := from >> 6
	wordTo := (to + 63) >> 6
	for w := wordFrom; w < wordTo; w++ {
		// Summary skip: jump over 64 consecutive zero detail words when
		// the current word is at a summary-aligned boundary.
		if w&63 == 0 && b.summary[w>>6] == 0 {
			w += 63 // outer loop adds 1
			continue
		}
		dw := readDetailWord(b.detail, int(w))
		if dw == 0 {
			continue
		}
		// Mask off bits before `from` in the first word.
		if w == wordFrom {
			fromBit := uint(from & 63)
			dw &^= (uint64(1) << fromBit) - 1
		}
		// Mask off bits at or after `to` in the last word.
		if w == wordTo-1 {
			toBit := uint(to & 63)
			if toBit != 0 {
				dw &= (uint64(1) << toBit) - 1
			}
		}
		if dw == 0 {
			continue
		}
		bit := bits.TrailingZeros64(dw)
		page := w<<6 + uint64(bit)
		if page < b.firstDataPage || page >= to {
			continue
		}
		return page, true
	}
	return 0, false
}

// runForward returns the lowest start of a run of n consecutive set bits
// fully contained in [from, to). Implements the spec algorithm: word-by-
// word scan, math/bits primitives for run-length within and across word
// boundaries.
func (b *Bitmap) runForward(from, to uint64, n int) (uint64, bool) {
	from = max(from, b.firstDataPage)
	to = min(to, b.totalPages)
	if from >= to || uint64(n) > to-from {
		return 0, false
	}
	wordFrom := from >> 6
	wordTo := (to + 63) >> 6

	runLen := 0
	var runStart uint64

	for w := wordFrom; w < wordTo; w++ {
		word := readDetailWord(b.detail, int(w))

		// Mask bits outside [from, to) at the edges. Edge masking forces
		// the run to break at the boundary, which is the semantic we
		// want — runs starting strictly before `from` are filtered out.
		if w == wordFrom {
			fromBit := uint(from & 63)
			word &^= (uint64(1) << fromBit) - 1
		}
		if w == wordTo-1 {
			toBit := uint(to & 63)
			if toBit != 0 {
				word &= (uint64(1) << toBit) - 1
			}
		}

		if word == 0 {
			runLen = 0
			continue
		}
		if word == ^uint64(0) {
			if runLen == 0 {
				runStart = w * 64
			}
			runLen += 64
			if runLen >= n {
				return runStart, true
			}
			continue
		}

		// Walk runs of 1-bits within the word. Each iteration either
		// breaks the current run (zeros) or extends/starts a run (ones).
		pos := 0
		for pos < 64 {
			rem := word >> pos
			if rem == 0 {
				break
			}
			zeros := bits.TrailingZeros64(rem)
			if zeros > 0 {
				runLen = 0
				pos += zeros
				if pos >= 64 {
					break
				}
				rem = word >> pos
			}
			// Count run of 1s starting at pos.
			ones := bits.TrailingZeros64(^rem)
			if pos+ones > 64 {
				ones = 64 - pos
			}
			if runLen == 0 {
				runStart = w*64 + uint64(pos)
			}
			runLen += ones
			if runLen >= n {
				return runStart, true
			}
			pos += ones
			// If pos < 64 here, the bit at pos is 0 (run of 1s ended).
			// The next loop iteration's `zeros` will clear runLen; no
			// need to clear it explicitly here.
		}

		// If the word ended before position 64, the run is broken at
		// the trailing zeros. If pos == 64 the run extends — carry it.
		if pos < 64 {
			runLen = 0
		}
	}

	return 0, false
}

// readDetailWord reads the i-th uint64 word from the detail byte slice.
// Used by every word-level scan; centralising keeps the LE convention in
// one place.
func readDetailWord(detail []byte, i int) uint64 {
	return le.Uint64(detail[i*8:])
}

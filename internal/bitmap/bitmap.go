package bitmap

import (
	"encoding/binary"
	"fmt"
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

// Snapshot is a copy of the Bitmap's mutable in-memory state at a
// point in time. Used by the pager to roll back tx-scoped mutations
// (AllocPage → bitmap.Clear, reclaimRPL → bitmap.Set, TailRefund →
// bitmap.Clear, loose→pendingFrees → bitmap.Set) when a commit aborts.
//
// The snapshot copies detail + summary + numFree + hint + dirty.
// Snapshot/Restore is O(detail-size); for a 256 GB MaxSize at 4 KB
// pages that is 8 MB. Acceptable cost on the rollback path; not taken
// on the commit hot path.
type Snapshot struct {
	detail  []byte
	summary []uint64
	numFree uint64
	hint    uint64
	dirty   map[uint32]struct{}
}

// Snapshot returns a copy of the Bitmap's mutable state.
func (b *Bitmap) Snapshot() *Snapshot {
	s := &Snapshot{
		detail:  slices.Clone(b.detail),
		summary: slices.Clone(b.summary),
		numFree: b.numFree,
		hint:    b.hint,
		dirty:   make(map[uint32]struct{}, len(b.dirty)),
	}
	for k := range b.dirty {
		s.dirty[k] = struct{}{}
	}
	return s
}

// Restore reverts the Bitmap to the captured snapshot. After Restore,
// b.detail, b.summary, b.numFree, b.hint, and b.dirty are byte- and
// element-identical to the snapshot. The bitmap struct is otherwise
// unchanged (pageSize, bitmapPages, firstDataPage, totalPages remain
// the same — they are configuration, not state).
func (b *Bitmap) Restore(s *Snapshot) {
	copy(b.detail, s.detail)
	// summary may be a different length only if pageSize changed,
	// which it doesn't; resize defensively.
	if len(b.summary) != len(s.summary) {
		b.summary = make([]uint64, len(s.summary))
	}
	copy(b.summary, s.summary)
	b.numFree = s.numFree
	b.hint = s.hint
	b.dirty = make(map[uint32]struct{}, len(s.dirty))
	for k := range s.dirty {
		b.dirty[k] = struct{}{}
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

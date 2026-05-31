package btree

import (
	"fmt"

	"github.com/thegrumpylion/gmdb/internal/page"
)

// trySplitLeafByGroup splits an overflowing compressed leaf at a restart-group
// boundary WITHOUT decoding entries — the no-decode fast path for the append
// case (page-formats.md §Leaf Split). It carves the CoW'd leaf (leftBuf/leftID)
// at the group boundary nearest 50% of its data bytes (page.FindSplitGroup),
// moves the right groups to a fresh page (page.SplitLeafRightHalf), appends the
// new (largest) entry to the right (page.TryAppend), and only then truncates the
// left in place (page.TruncateLeafToGroups). Both halves fit by
// construction — they are subsets of the already-fitting original leaf, and the
// right+new-entry is checked by TryAppend.
//
// Returns the parent separator, the right page ID, and ok=true on success.
// Declines (ok=false, no page leaked) when the leaf is not a multi-group
// compressed page, or the right half cannot absorb the new entry (a large inline
// append) — the caller then falls back to the byte-balanced decode split
// (findLeafSplitIndex), which can promote a value to overflow.
//
// Precondition: e.Key sorts after every key in leftBuf (the append case), and
// leftBuf is the freshly CoW'd, already-validated copy of the overflowing leaf
// (its page ID — leftID at the call site — becomes the left half).
func trySplitLeafByGroup(pw PageWriter, cfg page.Config, leftBuf []byte, e page.LeafEntry) (sep []byte, rightID uint64, ok bool, err error) {
	r := page.NewLeafReader(leftBuf, cfg)
	if !r.Compressed() || r.RestartCount() <= 1 {
		// Single group or uncompressed — nothing to carve; decode split.
		return nil, 0, false, nil
	}

	splitGroup := page.FindSplitGroup(leftBuf, cfg)

	rightID, err = pw.AllocPage()
	if err != nil {
		return nil, 0, false, fmt.Errorf("btree: alloc split-right leaf: %w", err)
	}
	rightBuf, err := pw.AllocSlab(rightID)
	if err != nil {
		_ = pw.FreePage(rightID)
		return nil, 0, false, fmt.Errorf("btree: alloc split-right buf: %w", err)
	}

	// Build the right half into the fresh page — READ-ONLY on leftBuf, so a
	// decline below leaves leftBuf intact for the decode-split fallback (which
	// reads that same buffer).
	leftCount, _ := page.SplitLeafRightHalf(leftBuf, rightBuf, cfg, splitGroup)

	// Append the new (largest) entry to the right half. If it doesn't fit even
	// after the split (a near-page inline value), decline so the decode split —
	// which can promote it to an overflow chain — handles the size-skewed case.
	// leftBuf is still untouched at this point.
	rightLast, _ := page.NewLeafReader(rightBuf, cfg).LastKey(nil)
	if !page.TryAppend(rightBuf, cfg, e, rightLast) {
		_ = pw.FreePage(rightID)
		return nil, 0, false, nil
	}

	// Committed: now truncate leftBuf in place to the left half.
	page.TruncateLeafToGroups(leftBuf, cfg, splitGroup)

	// Separator: leftLast < sep <= rightFirst. rightFirst is the original
	// boundary entry (the appended e is the right's LAST key, not its first).
	// ShortestSeparator returns a fresh slice, so neither key is retained.
	leftLast, _ := page.NewLeafReader(leftBuf, cfg).EntryAt(leftCount-1, nil)
	firstRight, _ := page.NewLeafReader(rightBuf, cfg).EntryAt(0, nil)
	sep = page.ShortestSeparator(leftLast.Key, firstRight.Key)
	return sep, rightID, true, nil
}

// findLeafSplitIndex chooses the boundary at which an overflowing leaf's
// entries are split across two pages, per page-formats.md §Leaf Split: a
// byte-balanced point biased to ~50% of the page's usable bytes — NOT
// the entry-count midpoint.
//
// Splitting by count (`len(entries)/2`) is wrong for size-skewed leaves.
// A value is promoted to an overflow chain only when its inline encoding
// cannot fit an otherwise-empty page (see needsOverflow), so inline
// values can approach a full page. A leaf mixing small entries with the
// occasional large inline value then has count midpoints that place more
// than one page of bytes on a side — producing a spurious ErrKeyTooLarge
// on a valid Put or ErrCorrupted on a valid Delete, even though a
// feasible byte-balanced boundary exists.
//
// The returned index mid (1 <= mid <= len(entries)-1) partitions into
// left = entries[:mid] and right = entries[mid:], each guaranteed to fit
// a single page. ok=false means no such partition exists — the entry
// multiset genuinely cannot be stored in two leaf pages (a single
// over-capacity entry, or large inline entries whose every contiguous
// 2-partition overflows a half). Callers surface that as ErrKeyTooLarge
// (insert paths) or, on the redistribute path where a feasible boundary
// provably exists (the entries arrived from two valid sibling pages),
// ErrCorrupted.
//
// Determinism (page-formats.md §Leaf Split "deterministic encoding
// invariant"): the choice is a pure function of (entries, cfg), so the
// same inputs always yield the same split — required for Check() repair
// and recovery to re-encode byte-identically.
//
// b is a scratch builder the function Resets onto scratch (a PageSize
// buffer the caller owns and rebuilds afterwards); fill is measured
// through the real LeafBuilder so prefix compression and
// inline-vs-overflow entry sizes are exact.
func findLeafSplitIndex(b *page.LeafBuilder, scratch []byte, cfg page.Config, entries []page.LeafEntry) (mid int, ok bool) {
	n := len(entries)
	if n < 2 {
		return 0, false
	}
	target := cfg.UsableSpace() / 2

	// Greedily fill the left half toward ~50% of usable bytes. Stop at
	// the first entry that does not fit (left is maximal) or the first
	// boundary past the 50% mark. The i>0 guard prevents a single large
	// first entry from forcing a 1-entry left when filling more would
	// balance better.
	b.Reset(scratch, cfg)
	g := 0
	for i, e := range entries {
		if !b.AddEntry(e) {
			break
		}
		g = i + 1
		if i > 0 && b.FreeSpace() < target {
			break
		}
	}
	if g < 1 {
		return 0, false // entries[0] alone exceeds page capacity
	}
	if g > n-1 {
		g = n - 1 // reserve at least one entry for the right half
	}

	// left = entries[:g] fits by construction. If right fits too, g is
	// the balanced split point (the common case for moderately-sized
	// entries, and always reachable on the redistribute path).
	if leafEntriesFit(b, scratch, cfg, entries[g:]) {
		return g, true
	}

	// The right half overflows at the balanced boundary: shift the
	// boundary rightward (more on the left, less on the right). The left
	// half grows monotonically and the right half shrinks monotonically,
	// so the first boundary whose right half fits — while its left half
	// still fits — is the most-balanced feasible split. If the left half
	// overflows before the right half fits, no feasible two-page split
	// exists.
	for m := g + 1; m <= n-1; m++ {
		if !leafEntriesFit(b, scratch, cfg, entries[:m]) {
			break
		}
		if leafEntriesFit(b, scratch, cfg, entries[m:]) {
			return m, true
		}
	}
	return 0, false
}

// leafEntriesFit reports whether es encodes within a single leaf page,
// measured by building it through b on the caller's scratch buffer.
func leafEntriesFit(b *page.LeafBuilder, scratch []byte, cfg page.Config, es []page.LeafEntry) bool {
	b.Reset(scratch, cfg)
	for _, e := range es {
		if !b.AddEntry(e) {
			return false
		}
	}
	return true
}

// largestInlineEntry returns the index of the plain inline entry
// (Flags == 0) with the largest value — lowest index breaking ties — or
// -1 if no inline entry remains to promote. The Put split path uses it to
// pick a value to move to an overflow chain when a leaf is too
// size-skewed for any two-page split (limits.md §Maximum Value Size
// guarantees any value is storable): promoting the largest inline value
// frees the most leaf space, so the
// retry converges fastest, and the choice is a deterministic function of
// the entry set. Overflow / subpage / nested cells (Flags != 0) are not
// inline values and are skipped; -1 means the obstruction is an over-size
// key, a genuine ErrKeyTooLarge per limits.md §Maximum Key Size.
func largestInlineEntry(entries []page.LeafEntry) int {
	best, bestLen := -1, -1
	for i := range entries {
		if entries[i].Flags != 0 {
			continue
		}
		if len(entries[i].Value) > bestLen {
			bestLen = len(entries[i].Value)
			best = i
		}
	}
	return best
}

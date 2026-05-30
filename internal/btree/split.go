package btree

import "github.com/thegrumpylion/gmdb/internal/page"

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

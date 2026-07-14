package btree

import (
	"fmt"

	"github.com/greatliontech/gmdb/internal/page"
)

// trySplitLeafByGroup splits an overflowing leaf at a restart-group boundary
// (compressed) or entry boundary (uncompressed) WITHOUT decoding entries — the
// no-decode fast path for the append case (page-formats.md §Leaf Split). It
// carves the CoW'd leaf (leftBuf/leftID) at the boundary nearest 50% of its data
// bytes, moves the right side to a fresh page, appends the new (largest) entry to
// the right (page.TryAppend), and only then truncates the left in place. Both
// halves fit by construction — they are subsets of the already-fitting original
// leaf, and the right+new-entry is checked by TryAppend.
//
// Returns the parent separator, the right page ID, and ok=true on success.
// Declines (ok=false, no page leaked) when the leaf has <2 entries, is a
// single-group compressed page, or the right half cannot absorb the new entry (a
// large inline append) — the caller then falls back to the byte-balanced decode
// split (findLeafSplitIndex), which can promote a value to overflow.
//
// The carve is two-phase (Split*RightHalf is READ-ONLY on leftBuf; Truncate*
// mutates it only after TryAppend commits) so a decline leaves leftBuf intact for
// the decode-split fallback, which reads that same buffer.
//
// Precondition: e.Key sorts after every key in leftBuf (the append case), and
// leftBuf is the freshly CoW'd, already-validated copy of the overflowing leaf
// (its page ID — leftID at the call site — becomes the left half).
func trySplitLeafByGroup(pw PageWriter, cfg page.Config, leftBuf []byte, e page.LeafEntry) (sep []byte, rightID uint64, ok bool, err error) {
	r := page.NewLeafReader(leftBuf, cfg)
	if r.Count() <= 1 {
		return nil, 0, false, nil // nothing to split off
	}
	variant := r.Variant()
	if r.Compressed() && r.RestartCount() <= 1 {
		return nil, 0, false, nil // single group — decode split
	}

	// Boundary nearest 50% of data bytes (variant-specific).
	var boundary int
	switch variant {
	case page.TypeLeaf:
		boundary = page.FindSplitGroup(leftBuf, cfg)
	case page.TypeLeafSegregated:
		boundary = page.FindSegSplitGroup(leftBuf, cfg)
	default:
		boundary = page.FindUCSplitIndex(leftBuf, cfg)
	}

	rightID, err = pw.AllocPage()
	if err != nil {
		return nil, 0, false, fmt.Errorf("btree: alloc split-right leaf: %w", err)
	}
	rightBuf, err := pw.ZeroPage(rightID)
	if err != nil {
		_ = pw.FreePage(rightID)
		return nil, 0, false, fmt.Errorf("btree: alloc split-right buf: %w", err)
	}

	// Build the right half into the fresh page — READ-ONLY on leftBuf, so a
	// decline below leaves leftBuf intact for the decode-split fallback.
	var leftCount int
	switch variant {
	case page.TypeLeaf:
		leftCount, _ = page.SplitLeafRightHalf(leftBuf, rightBuf, cfg, boundary)
	case page.TypeLeafSegregated:
		leftCount, _ = page.SplitSegRightHalf(leftBuf, rightBuf, cfg, boundary)
	default:
		leftCount, _ = page.SplitUCRightHalf(leftBuf, rightBuf, cfg, boundary)
	}

	// Append the new (largest) entry to the right half. If it doesn't fit even
	// after the split (a near-page inline value), decline so the decode split —
	// which can promote it to an overflow chain — handles the size-skewed case.
	// leftBuf is still untouched at this point.
	rightLast, _ := page.NewLeafReader(rightBuf, cfg).LastKey(nil)
	if !page.TryAppend(rightBuf, cfg, e, rightLast) {
		_ = pw.FreePage(rightID)
		return nil, 0, false, nil
	}

	// Committed: now truncate leftBuf in place to the left half (variant-specific).
	switch variant {
	case page.TypeLeaf:
		page.TruncateLeafToGroups(leftBuf, cfg, boundary)
	case page.TypeLeafSegregated:
		page.TruncateSegToGroups(leftBuf, cfg, boundary)
	default:
		page.TruncateUCToEntries(leftBuf, cfg, boundary)
	}

	// Separator: leftLast < sep <= rightFirst. rightFirst is the original
	// boundary entry (the appended e is the right's LAST key, not its first).
	// Boundary entries may be overflow-key cells whose resident bytes tie —
	// shortestSeparatorEntries materializes their extents only on that tie.
	leftLast, _ := page.NewLeafReader(leftBuf, cfg).EntryAt(leftCount-1, nil)
	firstRight, _ := page.NewLeafReader(rightBuf, cfg).EntryAt(0, nil)
	sep, err = shortestSeparatorEntries(pw, cfg, leftLast, firstRight)
	if err != nil {
		// rightID is not returned on the error path — free it here;
		// the caller's error path frees leftID.
		_ = pw.FreePage(rightID)
		return nil, 0, false, err
	}
	return sep, rightID, true, nil
}

// shortestSeparatorEntries computes the shortest separator between two
// adjacent leaf entries over their FULL keys. The common case decides
// from resident bytes alone (the divergence lands within them); only a
// full resident tie — overflow-key neighbors sharing their first-T
// bytes, or a resident that is a strict prefix of the other's — forces
// key-extent materialization.
func shortestSeparatorEntries(pr PageReader, cfg page.Config, left, right page.LeafEntry) ([]byte, error) {
	lk, rk := left.Key, right.Key
	n := min(len(lk), len(rk))
	for i := range n {
		if lk[i] != rk[i] {
			// Divergence within resident bytes: sep = right[:i+1] is
			// valid against the FULL keys too (rk is a prefix of
			// right's full key; left's full key still diverges at i).
			sep := make([]byte, i+1)
			copy(sep, rk[:i+1])
			return sep, nil
		}
	}
	if !left.IsOverflowKey() && !right.IsOverflowKey() {
		return page.ShortestSeparator(lk, rk), nil
	}
	fl, err := materializeEntryKey(pr, cfg, left)
	if err != nil {
		return nil, err
	}
	fr, err := materializeEntryKey(pr, cfg, right)
	if err != nil {
		return nil, err
	}
	return page.ShortestSeparator(fl, fr), nil
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
	// entries).
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
		if entries[i].Flags&^page.CellFlagOverflowKey != 0 {
			continue
		}
		if len(entries[i].Value) > bestLen {
			bestLen = len(entries[i].Value)
			best = i
		}
	}
	return best
}

// findBranchSplitIndex chooses the boundary at which an overflowing branch's
// cells are split across two pages — the byte-balanced analogue of
// findLeafSplitIndex for branch (internal) nodes, per page-formats.md
// §Leaf Split (the ~50%-of-content bias + lower-index tiebreak) and
// §Separator Computation (the boundary cell is lifted as the parent
// separator).
//
// Branch split/redistribute LIFT the boundary cell rather than copying it:
// for the returned mid (1 <= mid <= len(cells)-1) the left branch keeps
// cells[:mid], the right branch gets cells[mid+1:], and cells[mid] is lifted
// to the parent as the separator (its Child becomes the right branch's
// leftmost).
//
// TWO size metrics, deliberately distinct (page-formats.md §Plain Branch /
// §Segregated Branch + range-delete.md §Invariants):
//
//   - FIT (constraint): each half must encode within one physical page, so
//     the constraint uses the PHYSICAL page.BranchEncodedSize — this is
//     what EncodeBranch will actually write, so the "halves fit" decision
//     can't skew from the encoder. Measured per candidate half rather than
//     via a prefix sum so the check stays correct under any branch
//     layout's sizing rules (the plain layout is additive; a
//     within-page-compressed layout is not).
//   - BALANCE (objective): among fitting boundaries, minimize the LOGICAL
//     (uncompressed) imbalance via page.BranchLogicalSize. The fill-floor is
//     a logical-content property, so balancing logical content is what keeps
//     a redistribute's two halves both above the floor (range-delete.md
//     §Invariants) where reachable; under a compressed layout a
//     physically-balanced split could pile the cheap same-cluster cells on
//     one side and leave the other logically underfull, spuriously tripping
//     the decline. Under the plain layout the two metrics coincide.
//
// Splitting by cell COUNT (len(cells)/2) is wrong for size-skewed branches:
// a count midpoint can pile a page-worth of long separators on one half (a
// spurious ErrKeyTooLarge on a valid Put, or ErrCorrupted on a valid Delete
// redistribute) even though a feasible balanced boundary exists.
//
// ok=false means no contiguous two-page partition fits — a single separator
// genuinely exceeds page capacity (unreachable under limits.md §Maximum Key
// Size; the caller surfaces ErrKeyTooLarge on the split path). On the branch
// redistribute path a feasible boundary provably exists (the cells arrived
// from two sibling branches that each already fit one page, with separators
// bounded by limits.md §Maximum Key Size), so ok=false there is ErrCorrupted;
// the LEAF redistribute path has no such guarantee (canonical re-encode of
// variant-migrated inputs is not monotone) and DECLINES on ok=false instead
// — see mergeOrRedistributeLeaves.
//
// Determinism (page-formats.md §Leaf Split deterministic-encoding
// invariant): a pure function of (cfg, cells); ties in balance resolve to
// the lower-index boundary (the strict-< update below).
//
// Cost is O(n²) — n lift positions, each sizing two halves in O(half).
// A prefix-sum would suffice for the additive plain layout, but the
// per-candidate measurement stays correct under any layout's sizing
// rules. n is a branch's cell count (tens at typical key sizes) and this
// runs only on split/redistribute, off the read hot path, so the
// quadratic scan is not optimized further.
func findBranchSplitIndex(cfg page.Config, cells []page.BranchCell) (mid int, ok bool) {
	n := len(cells)
	if n < 2 {
		return 0, false
	}
	ce := cfg.ContentEnd()

	// Scan every lift position. left = cells[:m], right = cells[m+1:] (the
	// boundary cell cells[m] is lifted to the parent, in neither half).
	// Constraint: both halves must FIT physically (compressed). Objective:
	// among fitting boundaries, smallest LOGICAL imbalance; strict < makes the
	// lowest-index boundary win ties.
	best, bestDiff := -1, 0
	for m := 1; m <= n-1; m++ {
		if page.BranchEncodedSize(cfg, cells[:m]) > ce || page.BranchEncodedSize(cfg, cells[m+1:]) > ce {
			continue // a half doesn't fit a physical page
		}
		diff := page.BranchLogicalSize(cells[:m]) - page.BranchLogicalSize(cells[m+1:])
		if diff < 0 {
			diff = -diff
		}
		if best < 0 || diff < bestDiff {
			best, bestDiff = m, diff
		}
	}
	if best < 0 {
		return 0, false
	}
	return best, true
}

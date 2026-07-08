package btree

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/thegrumpylion/gmdb/internal/page"
)

// PageWriter extends PageReader with the write-path operations
// btree mutations need. A writer adapter over the page store
// satisfies it (the root package adapts *pager.Pager).
//
// Lifecycle contract: every page the btree allocates is either
// (a) installed in the tree (chain reachable from the returned new
// rootID), or (b) freed (via FreePage for single pages, FreeRun
// for overflow chains) before Put returns. The writer owns the
// page-buffer storage; btree never holds a buffer past the call
// that produced it.
type PageWriter interface {
	PageReader

	// AllocPage returns a fresh page ID from the writer's
	// free-space allocator. The allocation source and ordering are
	// the writer's concern; btree treats the result as an opaque
	// fresh page.
	AllocPage() (uint64, error)

	// CopyPage returns a writable copy of srcID's bytes installed at
	// dstID (supplied by the caller's prior AllocPage). This is
	// btree's copy-on-write primitive: mutate the returned buffer,
	// never srcID.
	CopyPage(srcID, dstID uint64) ([]byte, error)

	// ZeroPage returns a fresh zero-filled writable buffer for id,
	// reading no source page. Used for newly-encoded pages (split
	// halves, a new root branch) with no prior on-disk content.
	ZeroPage(id uint64) ([]byte, error)

	// FreePage retires id. A page allocated earlier in this same
	// write operation becomes reusable immediately; a page that
	// predates this operation is reclaimed when the writer's
	// transaction commits.
	FreePage(id uint64) error

	// AllocContiguous returns the first page ID of a fresh
	// contiguous run of n pages. Used for overflow-chain allocation
	// per page-formats.md §Overflow Page (followers have no header
	// and must be addressable as firstID+1, ..., firstID+n-1).
	AllocContiguous(n uint32) (uint64, error)

	// ZeroPageRun returns fresh zero-filled writable buffers for the
	// n pages of a run previously reserved via AllocContiguous.
	// pages[i] is the buffer for firstID + uint64(i).
	ZeroPageRun(firstID uint64, n uint32) (pages [][]byte, err error)

	// FreeRun retires a contiguous run of n pages starting at
	// firstID. Each id in [firstID, firstID+n) is retired exactly
	// like an individual FreePage.
	FreeRun(firstID uint64, n uint32) error
}

// ErrKeyTooLarge is returned by Put when the key is too large even
// for an overflow-reference leaf entry (key + overflow-ref header
// exceeds a single-entry leaf page's capacity). Per limits.md
// §Maximum Key Size the cap is ~(PageSize-40)/2; the
// tree-depth bound lifts this further. Values exceeding inline
// capacity are automatically promoted to an overflow chain — the
// sentinel fires only on oversize KEYS, never on oversize values.
var ErrKeyTooLarge = errors.New("btree: key too large for overflow-reference leaf entry")

// pathFrame records one level of a descent path — shared by the put
// ascend pass and the cursor. pageID is the page descended through;
// childIdx is the index into the branch's (leftmost + cells) child
// array taken at that level (0 = leftmost, 1..N = cells[0..N-1].Child;
// for the put path it is the BranchSearch result the ascend pass uses
// to know which child pointer to update). A cursor's leaf frame —
// always the last element of its path — leaves childIdx unused.
type pathFrame struct {
	pageID   uint64
	childIdx uint16
}

// descendToLeafForKey performs the shared put-descent: walk from
// rootID toward key, validating each branch and recording the path,
// stopping at the first leaf-typed page. Returns the recorded path,
// the leaf's page id, and the leaf's (pre-CoW) buffer. Both put
// variants (putReportCore and PutEntry) start here; their leaf
// mutation phases differ by contract (internal vs caller-managed
// overflow-chain ownership) and deliberately stay separate.
func descendToLeafForKey(pw PageWriter, cfg page.Config, rootID uint64, key []byte) (path []pathFrame, leafID uint64, leafBuf []byte, err error) {
	path = make([]pathFrame, 0, 8)
	cur := rootID
	for depth := 0; depth <= MaxTreeDepth; depth++ {
		buf, e := pw.Page(cur)
		if e != nil {
			return nil, 0, nil, e
		}
		typ, _, _, _ := page.ReadHeader(buf)
		if page.IsLeafType(typ) {
			return path, cur, buf, nil
		}
		if typ != page.TypeBranch {
			return nil, 0, nil, fmt.Errorf("%w: page %d has unexpected type %d during put descent", ErrCorrupted, cur, typ)
		}
		if e := validateBranchPage(buf, cfg, cur); e != nil {
			return nil, 0, nil, e
		}
		i := page.BranchSearch(buf, cfg, key)
		next := page.BranchChildAt(buf, cfg, i)
		if next == 0 {
			return nil, 0, nil, fmt.Errorf("%w: null child pointer in branch %d during put descent", ErrCorrupted, cur)
		}
		path = append(path, pathFrame{pageID: cur, childIdx: i})
		cur = next
	}
	return nil, 0, nil, ErrTreeTooDeep
}

// Put inserts or updates key=value in the tree rooted at rootID.
// Returns the new rootID — the caller (keyspace descriptor update) records this in the keyspace descriptor and propagates
// the descriptor update via CoW to the meta page.
//
// Mutations:
//   - rootID==0 (empty tree): allocates a fresh leaf containing
//     just (key, value); returns the new leaf's pageID.
//   - rootID!=0, key exists: CoWs leaf, updates value in place
//     (or splits if the new value is bigger than the old's room).
//   - rootID!=0, key new: CoWs leaf, inserts entry; on overflow
//     splits leaf, propagates a separator up the path, splitting
//     branches as needed; if root splits, grows a new root.
//
// On error: any pages already allocated during this Put are freed
// via the pager's FreePage (they become loose pages for re-use
// within this tx). The returned rootID is meaningful only when
// err == nil; on err the caller retains the prior rootID.
//
// Leaf format. Every leaf produced by Put is built via
// page.LeafBuilder against cfg.RestartGroupTarget (compressed when
// ≥2 / 0, uncompressed when ==1 — see internal/page/leaf_builder.go
// for variant dispatch). The builder owns natural-break
// heuristics + table layout; btree treats leaves as opaque past
// the LeafReader / LeafBuilder interface.
func Put(pw PageWriter, cfg page.Config, rootID uint64, key, value []byte) (uint64, error) {
	newRoot, _, err := putReportCore(pw, cfg, rootID, key, value, false)
	return newRoot, err
}

// PutReportExisting is Put that additionally reports whether key was
// already present (a replace) rather than newly inserted — determined
// during the same single descent, so a caller that would otherwise do
// Has-then-Put collapses two descents into one with no TOCTOU window.
// The value is always written (replacing any existing value), exactly
// like Put.
func PutReportExisting(pw PageWriter, cfg page.Config, rootID uint64, key, value []byte) (newRoot uint64, existed bool, err error) {
	return putReportCore(pw, cfg, rootID, key, value, false)
}

// InsertIfAbsent inserts key=value only when key is absent and reports
// whether the insert happened (added). When key is already present it
// is a no-op: no page is allocated or written and the original rootID
// is returned with added=false. Single descent. This is the
// set-membership primitive — re-writing an existing member would
// needlessly CoW-churn pages, so unlike PutReportExisting it does not
// write on present (which would also orphan the rewritten pages if the
// caller discards the new root).
func InsertIfAbsent(pw PageWriter, cfg page.Config, rootID uint64, key, value []byte) (newRoot uint64, added bool, err error) {
	newRoot, existed, err := putReportCore(pw, cfg, rootID, key, value, true)
	return newRoot, !existed, err
}

// putReportCore is the shared single-descent implementation behind Put,
// PutReportExisting, and InsertIfAbsent. It returns the new rootID and
// whether key already existed. When insertOnly is true and key is
// present it returns (rootID, true, nil) WITHOUT allocating or writing
// any page (the InsertIfAbsent no-op-on-present contract); otherwise it
// performs the standard replace-or-insert.
func putReportCore(pw PageWriter, cfg page.Config, rootID uint64, key, value []byte, insertOnly bool) (newRoot uint64, existed bool, err error) {
	// Split-safety key bound (limits.md §Maximum Key Size): a branch
	// must hold TWO full separators of this key. This is the spec's
	// stated bound (~(PageSize-40)/2), enforced at the entry gate so
	// acceptance is uniform across Put, PutEntry, and the bulk
	// builders — the split machinery itself tolerates single-cell
	// halves, so this is spec conformance, not a crash guard.
	if !branchHoldsTwoSeparators(cfg, len(key)) {
		return 0, false, ErrKeyTooLarge
	}
	if rootID == 0 {
		nr, e := putEmpty(pw, cfg, key, value)
		return nr, false, e
	}

	// Phase 1: the shared descent. The leaf buffer is retained for the
	// shared read (validate + search + last-key) below.
	path, leafID, leafBuf, err := descendToLeafForKey(pw, cfg, rootID, key)
	if err != nil {
		return 0, false, err
	}

	// Read the original leaf once — validate, then locate the key. Shared by
	// the InsertIfAbsent existence check, the append fast path, and the slow
	// path's insertion. Validating the original here also covers the slow path
	// (its CoW'd copy is byte-identical), so the decode below skips a second
	// Validate. All reads target the original buffer (valid pre-CoW); the
	// mutation phase writes only the CoW destination.
	rdr := page.NewLeafReader(leafBuf, cfg)
	if e := rdr.Validate(); e != nil {
		return 0, false, fmt.Errorf("%w: leaf %d: %w", ErrCorrupted, leafID, e)
	}
	searchIdx, _, searchFound := rdr.SearchLeaf(key)

	// InsertIfAbsent no-op-on-present: skip the write entirely (no CoW, no
	// alloc) when the key already exists.
	if insertOnly && searchFound {
		return rootID, true, nil
	}

	// Capture the last key before the CoW for the append fast path — the
	// original buffer is the safe source for prefix-delta encoding while the
	// CoW destination is the write target. lkScratch keeps the common case
	// (key ≤ 256 B) allocation-free; bytes.Clone gives independent ownership
	// across the CoW + splice regardless of where LastKey sourced the bytes.
	var prevKey []byte
	leafCount := rdr.Count()
	appendPos := !searchFound && leafCount > 0 && searchIdx == leafCount
	if appendPos {
		var lkScratch [256]byte
		lk, _ := rdr.LastKey(lkScratch[:0])
		prevKey = bytes.Clone(lk)
	}

	// Phase 2: leaf mutation. CoW the leaf; then either splice the new entry
	// in place (append fast path) or decode + re-encode (slow path).
	leftID, err := pw.AllocPage()
	if err != nil {
		return 0, false, fmt.Errorf("btree: alloc CoW leaf: %w", err)
	}
	leftBuf, err := pw.CopyPage(leafID, leftID)
	if err != nil {
		return 0, false, fmt.Errorf("btree: CoW leaf: %w", err)
	}

	// Build the entry to insert. A large value gets promoted to an overflow
	// chain here, allocated BEFORE the leaf write so a chain-alloc failure
	// rolls back via FreeRun without leaving the leaf in a half-written state.
	newEntry, err := buildPutEntry(pw, cfg, key, value)
	if err != nil {
		_ = pw.FreePage(leftID)
		return 0, false, err
	}

	// Helper: rollback the freshly-allocated new chain (if any) plus any
	// inline values promoted to overflow during a size-skewed split (the
	// store loop below) on failure paths. Mirrors the
	// AllocPage→CoW→mutate→FreePage ordering for chains. promotedChains is
	// empty until the first promotion, so earlier callers are no-ops.
	var promotedChains []page.LeafEntry
	rollbackNewChain := func() {
		if newEntry.IsOverflow() {
			runLen := page.OverflowRunLength(cfg, newEntry.TotalLen)
			_ = pw.FreeRun(newEntry.OverflowPage, runLen)
		}
		for _, pe := range promotedChains {
			runLen := page.OverflowRunLength(cfg, pe.TotalLen)
			_ = pw.FreeRun(pe.OverflowPage, runLen)
		}
	}

	// Append fast path: when the key sorts after every existing entry, splice
	// it in place instead of decoding + re-encoding the whole leaf
	// (page-formats.md §Insert and Delete). TryAppend leaves leftBuf
	// byte-unchanged on decline (page-full / unsupported variant), so we fall
	// through to the slow path. A pure insert displaces nothing — no old
	// overflow chain to free — and existed is false.
	if appendPos && page.TryAppend(leftBuf, cfg, newEntry, prevKey) {
		if e := pw.FreePage(leafID); e != nil {
			_ = pw.FreePage(leftID)
			rollbackNewChain()
			return 0, false, fmt.Errorf("btree: free old leaf %d: %w", leafID, e)
		}
		nr, e := ascendNoSplit(pw, cfg, path, leftID)
		if e != nil {
			_ = pw.FreePage(leftID)
			rollbackNewChain()
			return 0, false, e
		}
		return nr, false, nil
	}

	// Append-overflow group-split fast path: an append that overflowed a
	// multi-group compressed leaf splits at a group boundary WITHOUT decoding —
	// carve the page into two and append the new (largest) entry to the right
	// (page-formats.md §Leaf Split). Declines (→ slow decode split) on a single
	// group, an uncompressed/variant page, or when the right half can't absorb
	// the new entry; the decode split then does the byte-balanced (entry-precise)
	// split, promoting a near-page inline value to overflow if needed.
	if appendPos {
		sep, rightID, ok, e := trySplitLeafByGroup(pw, cfg, leftBuf, newEntry)
		if e != nil {
			_ = pw.FreePage(leftID)
			rollbackNewChain()
			return 0, false, e
		}
		if ok {
			if e := pw.FreePage(leafID); e != nil {
				_ = pw.FreePage(leftID)
				_ = pw.FreePage(rightID)
				rollbackNewChain()
				return 0, false, fmt.Errorf("btree: free old leaf %d: %w", leafID, e)
			}
			nr, e := ascendWithSplit(pw, cfg, path, leftID, sep, rightID)
			if e != nil {
				_ = pw.FreePage(leftID)
				_ = pw.FreePage(rightID)
				rollbackNewChain()
				return 0, false, e
			}
			return nr, false, nil
		}
	}

	// Mid-page insert fast path: when the key sorts strictly inside the leaf
	// (not an append, not a replace), splice it into its containing group in
	// place (page-formats.md §Insert and Delete) instead of decoding the whole
	// leaf. TryInsertAt leaves leftBuf byte-unchanged on decline (group at its
	// growth cap, page-full, or variant mismatch), so we fall through to the
	// slow path. A pure insert displaces nothing — no old chain to free,
	// existed is false.
	if !searchFound && searchIdx < leafCount && page.TryInsertAt(leftBuf, cfg, searchIdx, newEntry) {
		if e := pw.FreePage(leafID); e != nil {
			_ = pw.FreePage(leftID)
			rollbackNewChain()
			return 0, false, fmt.Errorf("btree: free old leaf %d: %w", leafID, e)
		}
		nr, e := ascendNoSplit(pw, cfg, path, leftID)
		if e != nil {
			_ = pw.FreePage(leftID)
			rollbackNewChain()
			return 0, false, e
		}
		return nr, false, nil
	}

	// Slow path: decode the leaf, insert/replace, re-build into the CoW
	// destination (or split into two). The original was validated above and
	// the CoW'd copy is byte-identical, so the decode skips re-validation.
	// insertOrReplaceLeaf returns the displaced entry (zero-valued on insert)
	// and whether the key existed (replace vs insert), so we can free its
	// overflow chain after the new leaf is committed and report existence to
	// PutReportExisting / InsertIfAbsent.
	entries := leafEntriesDeepCopyFrom(page.NewLeafReader(leftBuf, cfg))
	var displaced page.LeafEntry
	entries, displaced, existed = insertOrReplaceLeaf(entries, newEntry)

	// Store entries into one leaf if they fit, else a byte-balanced
	// two-page split (page-formats.md §Leaf Split — NOT the entry-count
	// midpoint: a leaf mixing small entries with large inline values has
	// count midpoints that place more than a page of bytes on one side,
	// since needsOverflow keeps a value inline until it cannot fit an
	// otherwise-empty page). If the entries are too size-skewed for any
	// two-page partition, promote the largest inline value to an overflow
	// chain — shrinking its leaf entry to a small reference — and retry.
	// limits.md §Maximum Value Size guarantees any value is storable, so
	// this keeps strict-fit inline for the common case yet never rejects a
	// valid Put; only an over-size key (no inline value left to promote)
	// yields ErrKeyTooLarge.
	b := page.NewLeafBuilder(leftBuf, cfg)
	for {
		// Attempt single-page build into leftBuf. AddEntry returns false
		// on page-full with no partial mutation committed (the fit check
		// fires before any byte is written, per leaf_builder.go).
		b.Reset(leftBuf, cfg)
		fits := true
		for _, e := range entries {
			if !b.AddEntry(e) {
				fits = false
				break
			}
		}
		if fits {
			b.Finish()
			// Post-build cleanup ordering: free the displaced chain FIRST
			// while the OLD leaf still references it (so a chain-free fault
			// can't observably orphan the chain — the entry is still
			// reachable from the not-yet-retired old leaf), then free the
			// old leaf. On either failure roll back the new state (leftID +
			// the new/promoted chains) so the "any pages allocated during
			// this Put are freed on error" contract holds.
			if err := freeOverflowChainIfPresent(pw, cfg, displaced); err != nil {
				_ = pw.FreePage(leftID)
				rollbackNewChain()
				return 0, false, err
			}
			if err := pw.FreePage(leafID); err != nil {
				_ = pw.FreePage(leftID)
				rollbackNewChain()
				return 0, false, fmt.Errorf("btree: free old leaf %d: %w", leafID, err)
			}
			nr, e := ascendNoSplit(pw, cfg, path, leftID)
			if e != nil {
				_ = pw.FreePage(leftID)
				rollbackNewChain()
				return 0, false, e
			}
			return nr, existed, nil
		}

		// Too big for one leaf: byte-balanced split.
		mid, ok := findLeafSplitIndex(b, leftBuf, cfg, entries)
		if !ok {
			// No two-page partition fits (size-skewed leaf). Promote the
			// largest inline value to overflow and retry — its leaf entry
			// shrinks to a small reference, freeing space so the set fits
			// one page or splits. A deterministic choice (largest, lowest
			// index on ties), so Check()/recovery re-encode preserves it.
			pi := largestInlineEntry(entries)
			if pi < 0 {
				_ = pw.FreePage(leftID)
				rollbackNewChain()
				return 0, false, ErrKeyTooLarge
			}
			promoted, perr := writeOverflowChain(pw, cfg, entries[pi].Key, entries[pi].Value)
			if perr != nil {
				_ = pw.FreePage(leftID)
				rollbackNewChain()
				return 0, false, perr
			}
			entries[pi] = promoted
			promotedChains = append(promotedChains, promoted)
			continue
		}

		// Emit the left half into leftBuf (the split index guarantees
		// entries[:mid] fits; the guard is defense in depth). LeafBuilder
		// writes from leafEntryStart forward and Finish zeros the unused
		// middle, so the page is byte-identical to a run on a fresh buffer.
		b.Reset(leftBuf, cfg)
		for _, e := range entries[:mid] {
			if !b.AddEntry(e) {
				_ = pw.FreePage(leftID)
				rollbackNewChain()
				return 0, false, ErrKeyTooLarge
			}
		}
		b.Finish()

		// Allocate + build right.
		rightID, err := pw.AllocPage()
		if err != nil {
			_ = pw.FreePage(leftID)
			rollbackNewChain()
			return 0, false, fmt.Errorf("btree: alloc split-right leaf: %w", err)
		}
		rightBuf, err := pw.ZeroPage(rightID)
		if err != nil {
			_ = pw.FreePage(leftID)
			_ = pw.FreePage(rightID)
			rollbackNewChain()
			return 0, false, fmt.Errorf("btree: alloc split-right buf: %w", err)
		}
		rb := page.NewLeafBuilder(rightBuf, cfg)
		for _, e := range entries[mid:] {
			if !rb.AddEntry(e) {
				_ = pw.FreePage(leftID)
				_ = pw.FreePage(rightID)
				rollbackNewChain()
				return 0, false, ErrKeyTooLarge
			}
		}
		rb.Finish()

		// Post-build cleanup ordering: free the displaced chain first
		// (still reachable via the not-yet-retired old leaf), then the
		// old leaf. On either failure roll back leftID + rightID + chains.
		if err := freeOverflowChainIfPresent(pw, cfg, displaced); err != nil {
			_ = pw.FreePage(leftID)
			_ = pw.FreePage(rightID)
			rollbackNewChain()
			return 0, false, err
		}
		if err := pw.FreePage(leafID); err != nil {
			_ = pw.FreePage(leftID)
			_ = pw.FreePage(rightID)
			rollbackNewChain()
			return 0, false, fmt.Errorf("btree: free old leaf %d: %w", leafID, err)
		}

		// Separator: shortest S with leftLast < S <= rightFirst.
		sep := page.ShortestSeparator(entries[mid-1].Key, entries[mid].Key)
		nr, e := ascendWithSplit(pw, cfg, path, leftID, sep, rightID)
		if e != nil {
			_ = pw.FreePage(leftID)
			_ = pw.FreePage(rightID)
			rollbackNewChain()
			return 0, false, e
		}
		return nr, existed, nil
	}
}

// buildPutEntry constructs the LeafEntry for a Put: an inline entry
// when the value fits, or an overflow-referencing entry (with the
// chain freshly allocated + encoded) when the inline form exceeds
// single-entry leaf capacity. Returns ErrKeyTooLarge if even the
// overflow-reference form doesn't fit (key alone is too large).
func buildPutEntry(pw PageWriter, cfg page.Config, key, value []byte) (page.LeafEntry, error) {
	if !needsOverflow(cfg, key, value) {
		return page.LeafEntry{Key: key, Value: value}, nil
	}
	if !overflowRefFitsLeaf(cfg, key) {
		return page.LeafEntry{}, ErrKeyTooLarge
	}
	return writeOverflowChain(pw, cfg, key, value)
}

// putEmpty allocates a single-leaf root containing just (key,
// value). The genesis path for an empty tree. Routes through
// buildPutEntry so a large value gets overflow-promoted the same
// way as a non-empty-tree Put.
func putEmpty(pw PageWriter, cfg page.Config, key, value []byte) (uint64, error) {
	newEntry, err := buildPutEntry(pw, cfg, key, value)
	if err != nil {
		return 0, err
	}
	id, err := pw.AllocPage()
	if err != nil {
		if newEntry.IsOverflow() {
			_ = pw.FreeRun(newEntry.OverflowPage, page.OverflowRunLength(cfg, newEntry.TotalLen))
		}
		return 0, fmt.Errorf("btree: alloc genesis leaf: %w", err)
	}
	buf, err := pw.ZeroPage(id)
	if err != nil {
		_ = pw.FreePage(id)
		if newEntry.IsOverflow() {
			_ = pw.FreeRun(newEntry.OverflowPage, page.OverflowRunLength(cfg, newEntry.TotalLen))
		}
		return 0, fmt.Errorf("btree: alloc genesis slab: %w", err)
	}
	b := page.NewLeafBuilder(buf, cfg)
	if !b.AddEntry(newEntry) {
		_ = pw.FreePage(id)
		if newEntry.IsOverflow() {
			_ = pw.FreeRun(newEntry.OverflowPage, page.OverflowRunLength(cfg, newEntry.TotalLen))
		}
		// Genesis single entry must fit by construction (overflow
		// promotion sized it down to a small reference); reaching
		// this branch implies an oversize key past
		// overflowRefFitsLeaf's check — keep ErrKeyTooLarge as the
		// surface.
		return 0, ErrKeyTooLarge
	}
	b.Finish()
	return id, nil
}

// readLeafEntriesDeepCopy reads every entry of leaf `buf` into a
// fresh LeafEntry slice with independently-allocated Key and Value
// bytes. Validates the leaf at the boundary and surfaces structural
// faults as ErrCorrupted.
//
// **Deep copy boundary.** LeafReader returns entries whose Key
// aliases the iterator's keyBuf (for compressed delta entries) or
// the page buffer (for restart entries and uncompressed entries);
// Value always aliases the page buffer. The btree's CoW-then-
// re-build flow reuses the SAME buffer as both decode source and
// builder destination (the CoW'd leaf becomes the new leaf's
// scratch). LeafBuilder writes entries from leafEntryStart forward
// and zeros the unused middle on Finish — so any borrowed bytes
// would be clobbered before the builder finished reading them. The
// per-entry bytes.Clone here is the aliasing-safe boundary that
// lets the post-decode entry slice survive arbitrarily many builder
// passes into the source buffer.
func readLeafEntriesDeepCopy(buf []byte, cfg page.Config, pageID uint64) ([]page.LeafEntry, error) {
	r := page.NewLeafReader(buf, cfg)
	if err := r.Validate(); err != nil {
		return nil, fmt.Errorf("%w: leaf %d: %w", ErrCorrupted, pageID, err)
	}
	return leafEntriesDeepCopyFrom(r), nil
}

// leafEntriesDeepCopyFrom decodes every entry of an ALREADY-VALIDATED leaf
// reader into a fresh LeafEntry slice with independently-allocated Key and
// Value bytes. Split out of readLeafEntriesDeepCopy so a caller that already
// validated the page (the Put slow path, which shares the append fast path's
// Validate) does not pay a second Validate. The deep-copy aliasing-safety
// rationale is documented on readLeafEntriesDeepCopy above.
func leafEntriesDeepCopyFrom(r page.LeafReader) []page.LeafEntry {
	out := make([]page.LeafEntry, 0, r.Count())
	it := r.IterForReuse(nil, nil, nil)
	for {
		e, ok := it.Next()
		if !ok {
			break
		}
		e.Key = bytes.Clone(e.Key)
		e.Value = bytes.Clone(e.Value)
		out = append(out, e)
	}
	return out
}

// insertOrReplaceLeaf finds the position of `newEntry.Key` in the
// sorted-by-key entries slice and either replaces the entry there
// (key exists) or inserts newEntry at the correct sorted position.
// Returns the modified slice plus the DISPLACED entry — non-zero
// on replace (the LeafEntry that was at the replaced slot, used
// by the caller to free any owned overflow chain) and zero-valued
// on insert.
//
// The original entries slice may be shared with the caller — do
// not retain. The replace path overwrites the slot wholesale so a
// stale Flags / OverflowPage / TotalLen from the old entry doesn't
// survive into the rebuilt leaf; the displaced entry is returned
// separately so the chain-free path runs on the OLD overflow info.
func insertOrReplaceLeaf(entries []page.LeafEntry, newEntry page.LeafEntry) ([]page.LeafEntry, page.LeafEntry, bool) {
	lo, hi := 0, len(entries)
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		cmp := bytes.Compare(entries[mid].Key, newEntry.Key)
		switch {
		case cmp < 0:
			lo = mid + 1
		case cmp > 0:
			hi = mid
		default:
			displaced := entries[mid]
			entries[mid] = newEntry
			return entries, displaced, true
		}
	}
	entries = append(entries, page.LeafEntry{})
	copy(entries[lo+1:], entries[lo:])
	entries[lo] = newEntry
	return entries, page.LeafEntry{}, false
}

// ascendNoSplit walks the path in reverse, CoWing each branch and
// updating the child pointer at childIdx from the old leafID to
// newChildID (which propagates up: each level's CoW produces a new
// pageID that becomes the next level's newChildID).
//
// For the no-split path, no separator is inserted — only the child
// pointer at childIdx is overwritten.
//
// Returns the new rootID. For an empty path (root is the leaf
// itself), returns newChildID directly.
func ascendNoSplit(pw PageWriter, cfg page.Config, path []pathFrame, newChildID uint64) (uint64, error) {
	cur := newChildID
	// Walk from leaf-side to root.
	for i := len(path) - 1; i >= 0; i-- {
		f := path[i]
		newBranchID, err := pw.AllocPage()
		if err != nil {
			return 0, fmt.Errorf("btree: alloc branch CoW: %w", err)
		}
		buf, err := pw.CopyPage(f.pageID, newBranchID)
		if err != nil {
			return 0, fmt.Errorf("btree: CoW branch %d: %w", f.pageID, err)
		}
		// Re-write the child pointer at childIdx.
		if err := branchReplaceChild(buf, cfg, f.childIdx, cur); err != nil {
			return 0, fmt.Errorf("btree: replace child in branch %d: %w", f.pageID, err)
		}
		if err := pw.FreePage(f.pageID); err != nil {
			return 0, fmt.Errorf("btree: free old branch %d: %w", f.pageID, err)
		}
		cur = newBranchID
	}
	return cur, nil
}

// ascendWithSplit walks the path in reverse, CoWing each branch
// and propagating a (separator, rightChildID) pair upward.
//
// At each level, the existing branch is CoW'd. The child pointer
// at childIdx is updated from the old leaf/branch ID to leftID
// (the existing-tree side of the split). The (sep, rightID) pair
// is inserted at childIdx+1.
//
// If the resulting branch overflows, the branch splits in two:
// the lower half stays as the CoW'd page, the upper half goes to
// a freshly allocated branch, and a new separator + rightID is
// computed for the NEXT level up.
//
// If the path is empty (the root itself was a leaf that split), a
// new root branch is allocated with two children: leftID and
// rightID, separator sep.
func ascendWithSplit(pw PageWriter, cfg page.Config, path []pathFrame, leftID uint64, sep []byte, rightID uint64) (uint64, error) {
	// ascendWithSplit is reached only after a leaf split (its two call
	// sites in putReportCore) — count that one leaf split here; each
	// branch split inside the loop is counted at its completion below
	// (TxStats.Splits).
	recordSplit(pw)
	for i := len(path) - 1; i >= 0; i-- {
		f := path[i]
		newBranchID, err := pw.AllocPage()
		if err != nil {
			return 0, fmt.Errorf("btree: alloc branch CoW (split): %w", err)
		}
		buf, err := pw.CopyPage(f.pageID, newBranchID)
		if err != nil {
			return 0, fmt.Errorf("btree: CoW branch %d (split): %w", f.pageID, err)
		}
		// Decode current branch, replace child at childIdx
		// with leftID, insert (sep, rightID) at childIdx+1.
		//
		// Deep-copy cell Keys: DecodeBranch returns Keys that
		// borrow from `buf`; we're about to re-encode INTO `buf`
		// (CoW destination doubles as source), and EncodeBranch
		// clears the buffer before writing. Without the deep
		// copy, the borrowed Keys go all-zero between decode and
		// encode-read. Same correctness boundary as
		// readLeafEntriesDeepCopy.
		leftmost, cells := page.DecodeBranch(buf, cfg)
		for i := range cells {
			cells[i].Key = bytes.Clone(cells[i].Key)
		}
		// Build newCells = cells[:childIdx] || (sep, rightID) ||
		// cells[childIdx:], then explicitly rewrite the
		// already-updated cell or the leftmost. Doing the Child
		// update on newCells (rather than on the source `cells`
		// slice) keeps the mutation independent of newCells's
		// backing-array aliasing — a future refactor that copies
		// cells field-by-field instead of via append won't
		// silently drop the Child update.
		newCells := make([]page.BranchCell, 0, len(cells)+1)
		newCells = append(newCells, cells[:f.childIdx]...)
		newCells = append(newCells, page.BranchCell{Key: sep, Child: rightID})
		newCells = append(newCells, cells[f.childIdx:]...)
		if f.childIdx == 0 {
			leftmost = leftID
		} else {
			newCells[f.childIdx-1].Child = leftID
		}

		// Try to encode in one branch.
		if err := page.EncodeBranch(buf, cfg, leftmost, newCells); err == nil {
			// Fits. Retire old branch; this CoW'd branch becomes
			// the child of the next-up level — switch to the
			// no-split ascend path for the remaining frames.
			if err := pw.FreePage(f.pageID); err != nil {
				return 0, fmt.Errorf("btree: free old branch %d: %w", f.pageID, err)
			}
			return ascendNoSplit(pw, cfg, path[:i], newBranchID)
		}

		// Branch split required. Choose the boundary by BYTE size, not the
		// cell-count midpoint: a branch mixing a few long (low-prefix-
		// sharing) separators with short ones has count midpoints that put
		// more than one page of cells on a side — a spurious failure on a
		// valid Put though a feasible byte-balanced boundary exists
		// (page-formats.md §Leaf Split; see findBranchSplitIndex). The
		// chosen halves are guaranteed to fit, so the EncodeBranch calls
		// below cannot fail on size.
		mid, ok := findBranchSplitIndex(cfg, newCells)
		if !ok {
			// No feasible two-page partition: a single separator exceeds
			// page capacity. Unreachable under limits.md §Maximum Key Size
			// (per-key cap ≤ ~PageSize/2 guarantees ≥2 cells fit any branch,
			// so a byte-balanced boundary always exists here); defense in
			// depth against a future max-key-size relaxation.
			_ = pw.FreePage(newBranchID)
			return 0, ErrKeyTooLarge
		}
		// In a branch split, the lifted cell's Key becomes the
		// separator propagated to the next level up; the
		// lifted cell's Child becomes the leftmost child of the
		// new right branch. The left branch keeps cells [0:mid]
		// and its leftmost is unchanged. The right branch gets
		// cells [mid+1:] with leftmost = newCells[mid].Child.
		nextSep := newCells[mid].Key
		nextRightLeftmost := newCells[mid].Child
		leftCells := newCells[:mid]
		rightCells := newCells[mid+1:]

		newRightID, err := pw.AllocPage()
		if err != nil {
			_ = pw.FreePage(newBranchID)
			return 0, fmt.Errorf("btree: alloc split-right branch: %w", err)
		}
		newRightBuf, err := pw.ZeroPage(newRightID)
		if err != nil {
			_ = pw.FreePage(newBranchID)
			_ = pw.FreePage(newRightID)
			return 0, fmt.Errorf("btree: alloc split-right branch slab: %w", err)
		}
		if err := page.EncodeBranch(buf, cfg, leftmost, leftCells); err != nil {
			_ = pw.FreePage(newBranchID)
			_ = pw.FreePage(newRightID)
			return 0, fmt.Errorf("btree: encode left branch split: %w", err)
		}
		if err := page.EncodeBranch(newRightBuf, cfg, nextRightLeftmost, rightCells); err != nil {
			_ = pw.FreePage(newBranchID)
			_ = pw.FreePage(newRightID)
			return 0, fmt.Errorf("btree: encode right branch split: %w", err)
		}
		if err := pw.FreePage(f.pageID); err != nil {
			return 0, fmt.Errorf("btree: free old branch %d: %w", f.pageID, err)
		}
		recordSplit(pw) // branch divided into two (TxStats.Splits)
		// Loop up with the new (sep, right) pair.
		leftID = newBranchID
		sep = nextSep
		rightID = newRightID
	}

	// Path exhausted. Root grew: allocate a new root branch with
	// leftID as leftmost and one cell (sep, rightID).
	newRootID, err := pw.AllocPage()
	if err != nil {
		return 0, fmt.Errorf("btree: alloc new root branch: %w", err)
	}
	newRootBuf, err := pw.ZeroPage(newRootID)
	if err != nil {
		_ = pw.FreePage(newRootID)
		return 0, fmt.Errorf("btree: alloc new root slab: %w", err)
	}
	cells := []page.BranchCell{{Key: sep, Child: rightID}}
	if err := page.EncodeBranch(newRootBuf, cfg, leftID, cells); err != nil {
		_ = pw.FreePage(newRootID)
		return 0, fmt.Errorf("btree: encode new root branch: %w", err)
	}
	return newRootID, nil
}

// branchReplaceChild updates the child pointer at descent index i:
//   - i == 0 → leftmost (Ptr[0])
//   - 0 < i ≤ N → cell[i-1].Child
//
// In-place rewrite — the cell directory + key data stay put; only
// the 8-byte child pointer at the cell's tail is rewritten.
func branchReplaceChild(buf []byte, cfg page.Config, i uint16, child uint64) error {
	n := page.BranchCellCount(buf)
	if i == 0 {
		page.SetBranchLeftmostChild(buf, child)
		return nil
	}
	if i > n {
		return fmt.Errorf("btree: branchReplaceChild i=%d > count=%d", i, n)
	}
	page.SetBranchCellChild(buf, cfg, i-1, child)
	return nil
}

package gmdb

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/thegrumpylion/gmdb/internal/page"
)

// errBulkEntryTooLarge is an internal sentinel: a single leaf entry does
// not fit on an otherwise-empty leaf page. The bulk builder is fed
// already-fitting entries (the Keyspace layer promotes over-inline values
// to overflow-reference entries, and SetKeyspace bounds value size at the
// declaration layer), so this is a defensive guard against a caller bug,
// never a reachable in-spec input.
var errBulkEntryTooLarge = errors.New("gmdb: bulkload entry too large for an empty leaf page")

// bulkPageWriter is the slice of the pager surface the bottom-up bulk
// builder needs: allocate a fresh page ID and write a fully-formed page
// directly to disk (slab bypass). *pager.Pager satisfies it. Kept minimal
// so the builder is exercisable with a fake writer in tests and cannot
// touch the slab.
type bulkPageWriter interface {
	AllocPage() (uint64, error)
	WriteDirect(id uint64, buf []byte) error
}

// bulkBuilder constructs a B+tree bottom-up from a strictly-ascending
// stream of leaf entries, writing each completed page directly to disk
// via WriteDirect as it is finished (bulkload.md §Algorithm + §Slab
// Bypass). Memory is O(depth × pageSize): one in-progress page buffer per
// tree level plus one separator/cell set per level (each bounded by a
// page's worth of cells), independent of the number of entries.
//
// Inv-Builder (entailed, bulkload.md): the emitted tree satisfies the
// branch routing contract — every separator S between adjacent children
// satisfies max(left subtree) < S ≤ min(right subtree) — so btree.Get and
// btree.Cursor over the result return each key's value. Leaf-level
// separators are the prefix-truncated page.ShortestSeparator of (last key
// of the closed leaf, first key of the next leaf); branch-level
// separators bubble up unchanged, because the separator of the first
// child a new branch page receives is exactly the separator that page
// needs in its own parent (max(prev sibling subtree) < S ≤ min(this
// subtree) holds at every level).
type bulkBuilder struct {
	pw              bulkPageWriter
	cfg             page.Config
	emptyBranchSize int // BranchEncodedSize of a cell-less branch page

	// Leaf level (level 0).
	leaf        *page.LeafBuilder
	leafBuf     []byte
	leafHave    bool   // an in-progress leaf page holds ≥1 entry
	leafSep     []byte // separator routing to the in-progress leaf in level 0 (nil = first leaf)
	leafLast    []byte // last key added overall, aliasing leafLastBuf
	leafLastBuf []byte // reusable backing array for leafLast

	// Branch levels. levels[0] is the first branch level above the
	// leaves; levels[i] feeds levels[i+1]. The deepest non-empty level
	// with a single page is the root.
	levels []*bulkBranchLevel

	count uint64 // total entries added
}

// bulkBranchLevel is one in-progress branch level's state.
type bulkBranchLevel struct {
	buf       []byte
	have      bool              // an in-progress branch page exists
	leftmost  uint64            // leftmost child of the in-progress page
	cells     []page.BranchCell // (separator, child) for non-leftmost children
	sep       []byte            // separator routing to the in-progress page in its parent (nil = first page at this level)
	size      int               // running BranchEncodedSize of the in-progress page
	closedAny bool              // a page at this level was already closed+propagated (⇒ >1 page exists here)
}

// newBulkBuilder returns a builder that emits pages through pw using cfg
// (which carries the keyspace's PageSize / PageChecksum /
// RestartGroupTarget, so leaves match the keyspace's leaf encoding).
func newBulkBuilder(pw bulkPageWriter, cfg page.Config) *bulkBuilder {
	return &bulkBuilder{
		pw:              pw,
		cfg:             cfg,
		emptyBranchSize: page.BranchEncodedSize(cfg, nil),
	}
}

// add appends one leaf entry. Entries MUST arrive in strictly-ascending
// key order; a non-ascending key returns ErrBulkLoadOutOfOrder (surfacing
// the LeafBuilder's order precondition as an error rather than a panic).
// The entry's flags decide its leaf encoding (inline / overflow-ref /
// subpage / nested-tree), all opaque to the builder.
func (b *bulkBuilder) add(e page.LeafEntry) error {
	if b.leafLast != nil && bytes.Compare(b.leafLast, e.Key) >= 0 {
		return ErrBulkLoadOutOfOrder
	}
	if !b.leafHave {
		b.startLeaf()
		b.leafSep = nil // first leaf is the leftmost child of level 0
	}
	if b.leaf.AddEntry(e) {
		b.recordLeafKey(e.Key)
		return nil
	}
	// e did not fit the current leaf. The leaf must be non-empty —
	// otherwise e is too large for any leaf.
	if b.leaf.Count() == 0 {
		return fmt.Errorf("%w (key %d bytes)", errBulkEntryTooLarge, len(e.Key))
	}
	if err := b.closeLeaf(e.Key); err != nil {
		return err
	}
	if !b.leaf.AddEntry(e) {
		return fmt.Errorf("%w (key %d bytes)", errBulkEntryTooLarge, len(e.Key))
	}
	b.recordLeafKey(e.Key)
	return nil
}

// finish writes the last in-progress leaf and all in-progress branches up
// to the root, returning the root page ID and the total entry count. For
// zero entries it returns (0, 0, nil) — an empty keyspace stays empty.
func (b *bulkBuilder) finish() (rootID uint64, count uint64, err error) {
	if !b.leafHave {
		return 0, 0, nil
	}
	leafID, err := b.writeLeaf()
	if err != nil {
		return 0, 0, err
	}
	if len(b.levels) == 0 {
		// A single leaf is the whole tree.
		return leafID, b.count, nil
	}
	// Feed the final leaf into level 0, then finalise branch levels.
	if err := b.addLink(0, b.leafSep, leafID); err != nil {
		return 0, 0, err
	}
	for L := 0; L < len(b.levels); L++ {
		bl := b.levels[L]
		id, err := b.writeBranch(bl)
		if err != nil {
			return 0, 0, err
		}
		if L == len(b.levels)-1 && !bl.closedAny {
			// Deepest level holds a single page → it is the root.
			return id, b.count, nil
		}
		if err := b.addLink(L+1, bl.sep, id); err != nil {
			return 0, 0, err
		}
	}
	return 0, 0, fmt.Errorf("gmdb: bulkload finish did not resolve a root (internal invariant violated)")
}

// startLeaf (re)initialises the in-progress leaf builder over the reusable
// leaf buffer.
func (b *bulkBuilder) startLeaf() {
	if b.leafBuf == nil {
		b.leafBuf = make([]byte, b.cfg.PageSize)
		b.leaf = page.NewLeafBuilder(b.leafBuf, b.cfg)
	} else {
		b.leaf.Reset(b.leafBuf, b.cfg)
	}
	b.leafHave = true
}

// recordLeafKey stashes key as the running last-key (cloned, since the
// caller may reuse the slice) and bumps the entry count.
func (b *bulkBuilder) recordLeafKey(key []byte) {
	b.leafLastBuf = append(b.leafLastBuf[:0], key...)
	b.leafLast = b.leafLastBuf
	b.count++
}

// writeLeaf finalises the in-progress leaf, allocates a fresh page ID, and
// pwrites it directly. Returns the leaf's page ID.
func (b *bulkBuilder) writeLeaf() (uint64, error) {
	b.leaf.Finish()
	id, err := b.pw.AllocPage()
	if err != nil {
		return 0, err
	}
	if err := b.pw.WriteDirect(id, b.leafBuf); err != nil {
		return 0, err
	}
	b.leafHave = false
	return id, nil
}

// closeLeaf writes the in-progress leaf, propagates its (separator, id)
// link up to level 0, and starts a fresh leaf whose separator routes to it
// in level 0. nextFirstKey is the first key of the new leaf (the entry that
// did not fit the closed leaf); it is strictly greater than the closed
// leaf's last key, so ShortestSeparator's precondition holds.
func (b *bulkBuilder) closeLeaf(nextFirstKey []byte) error {
	closedSep := b.leafSep
	id, err := b.writeLeaf()
	if err != nil {
		return err
	}
	if err := b.addLink(0, closedSep, id); err != nil {
		return err
	}
	// The new leaf's separator: shortest S with last(closed) < S ≤
	// nextFirstKey. ShortestSeparator allocates a fresh slice, so it is
	// safe to retain past the caller's reuse of nextFirstKey.
	newSep := page.ShortestSeparator(b.leafLast, nextFirstKey)
	b.startLeaf()
	b.leafSep = newSep
	return nil
}

// addLink feeds a (separator, child) link into branch level `level`
// (0-based; levels[0] is the first branch level above the leaves). The
// separator slice is retained until the page is written, so it must be a
// fresh allocation the caller does not mutate (ShortestSeparator results
// and bubbled-up bl.sep both satisfy this).
func (b *bulkBuilder) addLink(level int, sep []byte, child uint64) error {
	for len(b.levels) <= level {
		b.levels = append(b.levels, &bulkBranchLevel{})
	}
	bl := b.levels[level]
	if !bl.have {
		b.startBranch(bl, sep, child)
		return nil
	}
	cost := b.branchCellCost(sep)
	if bl.size+cost <= b.cfg.ContentEnd() {
		bl.cells = append(bl.cells, page.BranchCell{Key: sep, Child: child})
		bl.size += cost
		return nil
	}
	// The in-progress page is full: write it, then start a new page whose
	// leftmost child is the link that did not fit, and propagate the
	// written page's own link up one level.
	closedSep := bl.sep
	id, err := b.writeBranch(bl)
	if err != nil {
		return err
	}
	bl.closedAny = true
	b.startBranch(bl, sep, child)
	return b.addLink(level+1, closedSep, id)
}

// startBranch (re)initialises bl as a new in-progress branch page with the
// given leftmost child and the separator routing to it in its parent. sep
// must already be a fresh, immutable slice (or nil for the first page at
// the level).
func (b *bulkBuilder) startBranch(bl *bulkBranchLevel, sep []byte, leftmost uint64) {
	if bl.buf == nil {
		bl.buf = make([]byte, b.cfg.PageSize)
	}
	bl.leftmost = leftmost
	bl.cells = bl.cells[:0]
	bl.sep = sep
	bl.size = b.emptyBranchSize
	bl.have = true
}

// writeBranch encodes bl's leftmost + cells into its buffer, allocates a
// fresh page ID, and pwrites it directly. Returns the branch's page ID.
func (b *bulkBuilder) writeBranch(bl *bulkBranchLevel) (uint64, error) {
	if err := page.EncodeBranch(bl.buf, b.cfg, bl.leftmost, bl.cells); err != nil {
		return 0, fmt.Errorf("gmdb: bulkload encode branch: %w", err)
	}
	id, err := b.pw.AllocPage()
	if err != nil {
		return 0, err
	}
	if err := b.pw.WriteDirect(id, bl.buf); err != nil {
		return 0, err
	}
	bl.have = false
	return id, nil
}

// branchCellCost is the increase in a branch page's encoded size from
// adding one cell with the given separator key.
func (b *bulkBuilder) branchCellCost(sep []byte) int {
	return page.BranchEncodedSize(b.cfg, []page.BranchCell{{Key: sep}}) - b.emptyBranchSize
}

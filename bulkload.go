package gmdb

import (
	"bytes"
	"errors"
	"fmt"
	"iter"

	"github.com/thegrumpylion/gmdb/internal/btree"
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

// bulkOverflowWriter is the pager surface the streaming overflow-chain
// writer needs: reserve a contiguous run of fresh page IDs and pwrite each
// directly (slab bypass). *pager.Pager satisfies it.
type bulkOverflowWriter interface {
	AllocContiguous(n uint32) (uint64, error)
	WriteDirect(id uint64, buf []byte) error
}

// writeBulkOverflowChain streams value into a fresh contiguous overflow
// run, pwriting each page directly via WriteDirect (slab bypass) using a
// single reused page buffer — O(pageSize) memory, never materializing the
// whole run (the BulkLoad memory contract, bulkload.md §Slab Bypass).
// Returns the run's first page ID; the leaf carries an overflow-reference
// entry to it.
//
// The on-disk layout is byte-identical to page.EncodeOverflowRun (first
// page: TypeOverflow header with AdditionalPages = runLen-1, then the
// value prefix; followers: raw value bytes), so the engine's overflow
// reader (page.AssembleOverflowValue) reassembles the value unchanged. The
// per-page value capacities exclude the checksum footer (page.ContentEnd),
// so WriteDirect's footer lands in the reserved tail without overwriting
// value bytes.
//
// Atomicity: a mid-run WriteDirect failure leaves the already-pwritten
// run pages as bounded leakage — they are at fresh IDs unreferenced by any
// recoverable meta and are reclaimed by tx rollback (AbortTx) or, on a
// committed-after-error orphan, by background maintenance — identical to
// every other bulk write (bulkload.md §Atomicity). No FreeRun is issued:
// the contract is that a BulkLoad error aborts the whole load.
func writeBulkOverflowChain(pw bulkOverflowWriter, cfg page.Config, value []byte) (uint64, error) {
	runLen := page.OverflowRunLength(cfg, uint64(len(value)))
	firstID, err := pw.AllocContiguous(runLen)
	if err != nil {
		return 0, fmt.Errorf("gmdb: bulkload alloc overflow run (%d pages): %w", runLen, err)
	}
	buf := make([]byte, cfg.PageSize)

	// First page: header + value prefix. buf is freshly zeroed, so the
	// region past the copied prefix (for a value shorter than firstCap)
	// is already zero-filled.
	page.WriteHeader(buf, page.TypeOverflow, 0, runLen-1)
	firstCap := page.OverflowFirstPageCapacity(cfg)
	off := copy(buf[page.HeaderSize:page.HeaderSize+firstCap], value)
	if err := pw.WriteDirect(firstID, buf); err != nil {
		return 0, fmt.Errorf("gmdb: bulkload write overflow first page: %w", err)
	}

	// Follower pages: raw value bytes, no header. clear(buf) each
	// iteration drops the previous page's content (incl. the footer
	// WriteDirect wrote) and zero-fills the trailing slack of the final
	// (partial) follower.
	followerCap := page.OverflowFollowerCapacity(cfg)
	for i := uint32(1); i < runLen; i++ {
		clear(buf)
		off += copy(buf[:followerCap], value[off:])
		if err := pw.WriteDirect(firstID+uint64(i), buf); err != nil {
			return 0, fmt.Errorf("gmdb: bulkload write overflow follower page %d: %w", i, err)
		}
	}
	return firstID, nil
}

// BulkLoad replaces the contents of an empty keyspace with the sorted
// key-value stream produced by rows. Input MUST be in strictly-ascending
// lex key order; a non-ascending key returns ErrBulkLoadOutOfOrder. The
// keyspace must be empty (Count == 0), else ErrBulkLoadNonEmpty —
// clear with ks.DeleteRange(nil, nil) first if necessary. Returns the
// number of pairs written. Per bulkload.md §API.
//
// BulkLoad bypasses the per-tx slab budget: pages are pwritten directly to
// fresh page IDs as they are constructed, so memory is O(depth × pageSize)
// independent of input size. Over-inline values are stored as overflow
// chains streamed the same way (no MaxTxBufferBytes charge). The new tree
// is published only at Tx.Commit via the meta swap; a mid-load crash or a
// rollback leaves the keyspace at its pre-BulkLoad state (bounded leakage
// reclaimed by background maintenance) per bulkload.md §Atomicity.
//
// Errors: ErrReadOnly (read-only tx or handle), ErrKeyspaceClosed (handle
// invalidated by a same-tx DeleteKeyspace), ErrBulkLoadNonEmpty,
// ErrBulkLoadOutOfOrder, ErrKeyEmpty (empty key in the stream), and the
// btree key-size / I/O errors.
func (ks *Keyspace) BulkLoad(rows iter.Seq2[[]byte, []byte]) (uint64, error) {
	if err := ks.tx.requireOpen(true); err != nil {
		return 0, err
	}
	if ks.dead {
		return 0, ErrKeyspaceClosed
	}
	if ks.readOnly {
		return 0, ErrReadOnly
	}
	if ks.desc.Count != 0 {
		return 0, ErrBulkLoadNonEmpty
	}
	if len(ks.indexes) > 0 {
		// Indexed-keyspace BulkLoad (extractor + per-index sort/spill +
		// unique detection) is wired in a later sub-chunk; until then
		// it errors rather than silently building rows without indexes.
		return ks.bulkLoadIndexed(rows)
	}

	cfg := ks.builderCfg()
	b := newBulkBuilder(ks.tx.pgr, cfg)
	var loopErr error
	rows(func(key, value []byte) bool {
		if len(key) == 0 {
			loopErr = ErrKeyEmpty
			return false
		}
		e, err := ks.bulkLeafEntry(cfg, key, value)
		if err != nil {
			loopErr = err
			return false
		}
		if err := b.add(e); err != nil {
			loopErr = err
			return false
		}
		return true
	})
	if loopErr != nil {
		return 0, loopErr
	}
	root, count, err := b.finish()
	if err != nil {
		return 0, err
	}
	// Retire any leftover root (a truly empty keyspace has Root == 0;
	// this is defense for an empty-but-allocated root) before publishing
	// the bulk-built tree. FreeSubtree(0) is a no-op.
	if _, err := btree.FreeSubtree(ks.tx.pgr, cfg, ks.desc.Root); err != nil {
		return 0, mapBtreeErr(err)
	}
	ks.desc.Root = root
	ks.desc.Count = count
	ks.markDirty()
	ks.markCursorsStale()
	return count, nil
}

// bulkLoadIndexed bulk-loads an indexed keyspace: it must build the row
// tree AND every index's data tree (extractor over each row, external
// sort with ScratchDir spill, unique-violation detection at the merge
// output) so the committed state has consistent rows + index entries. That
// path is implemented in a later sub-chunk; until then this returns an
// error rather than silently producing rows without index entries (which
// would leave the indexes permanently out of sync with the data).
func (ks *Keyspace) bulkLoadIndexed(rows iter.Seq2[[]byte, []byte]) (uint64, error) {
	_ = rows
	return 0, fmt.Errorf("gmdb: BulkLoad on an indexed keyspace requires the indexed bulk-load path (not yet wired)")
}

// bulkLeafEntry builds the leaf entry for one (key, value): an inline entry
// when it fits an empty leaf, otherwise a streamed overflow chain plus an
// overflow-reference entry. nil value is normalised to empty (the
// nil-value-as-empty invariant). The inline/overflow boundary is btree's
// (shared with Put) so a value Put would inline, BulkLoad inlines.
func (ks *Keyspace) bulkLeafEntry(cfg page.Config, key, value []byte) (page.LeafEntry, error) {
	if value == nil {
		value = []byte{}
	}
	if !btree.NeedsOverflow(cfg, key, value) {
		return page.LeafEntry{Key: key, Value: value}, nil
	}
	if !btree.OverflowRefFitsLeaf(cfg, key) {
		return page.LeafEntry{}, btree.ErrKeyTooLarge
	}
	firstID, err := writeBulkOverflowChain(ks.tx.pgr, cfg, value)
	if err != nil {
		return page.LeafEntry{}, err
	}
	return page.LeafEntry{
		Flags:        page.CellFlagOverflow,
		Key:          key,
		OverflowPage: firstID,
		TotalLen:     uint64(len(value)),
	}, nil
}

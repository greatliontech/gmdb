package gmdb

import (
	"bytes"
	"errors"
	"fmt"
	"iter"

	"github.com/greatliontech/gmdb/internal/btree"
	"github.com/greatliontech/gmdb/internal/page"
)

// errBulkEntryTooLarge is an internal sentinel: a single leaf entry does
// not fit on an otherwise-empty leaf page. On the KEYSPACE path it is a
// defensive guard against a caller/engine bug — the layer promotes
// over-inline values to overflow-reference entries and pre-checks the
// key (bulkLeafEntry → ErrKeyTooLarge). On the SET path it IS reachable
// in-spec for variable-size sets: a key's FIRST value bypasses the
// promotion threshold by design (matching Put's genesis shape, which
// never threshold-checks a single value), so an oversize first value —
// or an oversize member reaching a nested builder as its key — hits
// this guard; bulkMapEntryTooLarge translates it to the public
// ErrKeyTooLarge exactly as the same input surfaces through Put.
var errBulkEntryTooLarge = errors.New("gmdb: bulkload entry too large for an empty leaf page")

// bulkMapEntryTooLarge translates the builder's empty-leaf guard
// into btree.ErrKeyTooLarge wherever a USER-derived entry can reach
// it in-spec — set values and members (the first-value/nested cases
// in the sentinel doc). On the index-build boundary the guard is
// defensive only: bulkLeafEntry pre-checks extractor-produced keys
// against the two-separators bound and promotes over-inline covering
// payloads to overflow references, so an index entry that reaches
// the builder always fits an empty leaf. The BulkLoad boundary's
// mapBtreeErr then surfaces the public sentinel, matching what the
// same input yields through Put. Every other error passes through
// unchanged.
func bulkMapEntryTooLarge(err error) error {
	if err != nil && errors.Is(err, errBulkEntryTooLarge) {
		return fmt.Errorf("%w: %v", btree.ErrKeyTooLarge, err)
	}
	return err
}

// bulkPageWriter is the slice of the pager surface the bottom-up bulk
// builder needs: allocate a fresh page ID and write a fully-formed page
// directly to disk (slab bypass). *pager.Pager satisfies it. Kept minimal
// so the builder is exercisable with a fake writer in tests and cannot
// touch the slab.
type bulkPageWriter interface {
	AllocPage() (uint64, error)
	// AllocContiguous serves the branch builder's overflow-separator
	// key extents (page-formats.md §Overflow-Key Cells) in addition
	// to bulkOverflowWriter's value runs; both concrete writers (the
	// live pager and CopyTo's fresh-file writer) provide it.
	AllocContiguous(n uint32) (uint64, error)
	WriteDirect(id uint64, buf []byte) error
}

// bulkBuilder constructs a B+tree bottom-up from a strictly-ascending
// stream of leaf entries, writing each completed page directly to disk
// via WriteDirect as it is finished (bulkload.md §Algorithm + §Slab
// Bypass). Memory is O(depth × pageSize): one in-progress page buffer per
// tree level plus one separator/cell set per level (each bounded by a
// page's worth of cells), independent of the number of entries.
//
// Entailed (bulkload.md §Invariants): the emitted tree satisfies the
// branch routing contract — every separator S between adjacent children
// satisfies max(left subtree) < S ≤ min(right subtree) — so btree.Get and
// btree.Cursor over the result return each key's value. Leaf-level
// separators are the cross-level-truncated page.ShortestSeparator of (last key
// of the closed leaf, first key of the next leaf); branch-level
// separators bubble up unchanged, because the separator of the first
// child a new branch page receives is exactly the separator that page
// needs in its own parent (max(prev sibling subtree) < S ≤ min(this
// subtree) holds at every level).
type bulkBuilder struct {
	pw  bulkPageWriter
	cfg page.Config

	// Leaf level (level 0).
	leaf     *page.LeafBuilder
	leafBuf  []byte
	leafHave bool   // an in-progress leaf page holds ≥1 entry
	leafSep  []byte // separator routing to the in-progress leaf in level 0 (nil = first leaf)
	leafLast []byte // last key added overall; an owned clone (see add), valid until the next add

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
	keyLenSum int               // Σ len(cell.Key) of the in-progress page's cells
	extRefs   int               // count of overflow (key-extent-bearing) cells in the in-progress page
	closedAny bool              // a page at this level was already closed+propagated (⇒ >1 page exists here)
}

// newBulkBuilder returns a builder that emits pages through pw using cfg
// (which carries the keyspace's PageSize / PageChecksum /
// RestartGroupTarget, so leaves match the keyspace's leaf encoding).
func newBulkBuilder(pw bulkPageWriter, cfg page.Config) *bulkBuilder {
	return &bulkBuilder{
		pw:  pw,
		cfg: cfg,
	}
}

// add appends one leaf entry. Entries MUST arrive in strictly-ascending
// key order; a non-ascending key returns ErrBulkLoadOutOfOrder (surfacing
// the LeafBuilder's order precondition as an error rather than a panic).
// The entry's flags decide its leaf encoding (inline / overflow-ref /
// subpage / nested-tree), all opaque to the builder.
// add appends e to the in-progress leaf. fullKey is e's FULL key —
// identical to e.Key for inline entries; the complete (resident +
// extent) key for overflow-key entries, whose e.Key holds only the
// resident first-T slice (page-formats.md §Overflow-Key Cells). The
// ordering check and the separator computation both run over full
// keys: resident slices can tie between distinct overflow keys, which
// would trip the ascending check spuriously and — worse — compute
// truncated separators.
func (b *bulkBuilder) add(e page.LeafEntry, fullKey []byte) error {
	if b.leafLast != nil && bytes.Compare(b.leafLast, fullKey) >= 0 {
		return ErrBulkLoadOutOfOrder
	}
	// Clone both key views once and use the clones everywhere.
	// page.LeafBuilder retains e.Key by reference for its
	// ascending-order assertion (lastAddedKey borrows the caller's
	// slice, not the on-page copy — compressed leaves store only the
	// unshared suffix, so the full key cannot be recovered from the
	// page), and we retain the FULL key as leafLast for separator
	// computation. The iter.Seq2 input contract lets the caller reuse
	// the key buffer after yield returns, so borrowing would be a
	// use-after-free.
	k := bytes.Clone(fullKey)
	if len(e.Key) == len(fullKey) {
		e.Key = k
	} else {
		e.Key = k[:len(e.Key)] // resident slice of the same clone
	}
	if !b.leafHave {
		b.startLeaf()
		b.leafSep = nil // first leaf is the leftmost child of level 0
	}
	if b.leaf.AddEntry(e) {
		b.recordLeafKey(k)
		return nil
	}
	// e did not fit the current leaf. The leaf must be non-empty —
	// otherwise e is too large for any leaf.
	if b.leaf.Count() == 0 {
		return fmt.Errorf("%w (key %d bytes)", errBulkEntryTooLarge, len(k))
	}
	if err := b.closeLeaf(k); err != nil {
		return err
	}
	if !b.leaf.AddEntry(e) {
		return fmt.Errorf("%w (key %d bytes)", errBulkEntryTooLarge, len(k))
	}
	b.recordLeafKey(k)
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

// recordLeafKey stashes the owned key clone (from add) as the running
// last-key and bumps the entry count. The clone survives until the next
// add reassigns leafLast, covering both the separator computation and the
// LeafBuilder's borrowed lastAddedKey assertion on the following add.
func (b *bulkBuilder) recordLeafKey(key []byte) {
	b.leafLast = key
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
	// Sizing goes through page.BranchSizeOf, which dispatches on the
	// configured branch layout: plain is additive in (count,
	// keyLenSum, extRefs); segregated is non-additive in the shared
	// prefix — adding a separator that shares less prefix lengthens
	// every existing cell's stored suffix — so each append recomputes
	// prefixLen = commonPrefix(cells[0], newest) (cells arrive
	// ascending, making first-vs-newest the whole-set prefix).
	// Overflow separators (page-formats.md §Overflow-Key Cells): size
	// with the RESIDENT first-T slice + the 12-byte extent reference;
	// the extent is written only when the cell is actually appended,
	// so the page-full decline path (which re-threads sep as the next
	// page's parent separator) never leaks a run.
	t := b.cfg.InlineThreshold()
	resident := sep
	sepOvk := len(sep) > t
	if sepOvk {
		resident = sep[:t]
	}
	extRefs := bl.extRefs
	if sepOvk {
		extRefs++
	}
	newPrefixLen := len(resident)
	if len(bl.cells) > 0 {
		newPrefixLen = sharedLen(bl.cells[0].Key, resident)
	}
	if page.BranchSizeOf(b.cfg, len(bl.cells)+1, bl.keyLenSum+len(resident), newPrefixLen, extRefs) <= b.cfg.ContentEnd() {
		cell := page.BranchCell{Key: resident, Child: child}
		if sepOvk {
			ext, err := writeBulkOverflowChain(b.pw, b.cfg, sep[t:])
			if err != nil {
				return err
			}
			cell.KeyExtPage = ext
			cell.KeyTotalLen = uint32(len(sep))
		}
		bl.cells = append(bl.cells, cell)
		bl.keyLenSum += len(resident)
		bl.extRefs = extRefs
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
	bl.keyLenSum = 0
	bl.extRefs = 0
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
// On an indexed keyspace BulkLoad also builds every index's data tree: it
// runs each index's extractor on every row, externally sorts the produced
// entries (in memory, spilling to Options.ScratchDir when the aggregate
// exceeds MaxTxBufferBytes), and bulk-builds each index tree from the
// sorted stream — all published atomically with the row tree. A unique-index
// duplicate aborts the whole load with ErrIndexUniqueViolation and publishes
// nothing (see bulkload_indexed.go).
//
// Errors: ErrReadOnly (read-only tx or handle), ErrKeyspaceClosed (handle
// invalidated by a same-tx DeleteKeyspace), ErrBulkLoadNonEmpty,
// ErrBulkLoadOutOfOrder, ErrKeyEmpty (empty key in the stream),
// ErrIndexUniqueViolation (duplicate key in a unique index), a wrapped I/O
// error on a sort-spill failure, and the btree key-size / I/O errors.
func (ks *Keyspace) BulkLoad(rows iter.Seq2[[]byte, []byte]) (uint64, error) {
	if err := ks.requireWritable(); err != nil {
		return 0, err
	}
	if ks.desc.Count != 0 {
		return 0, ErrBulkLoadNonEmpty
	}
	if len(ks.indexes) > 0 {
		// Indexed keyspaces build the row tree AND every index data tree
		// (extractor per row + per-index external sort/spill + unique
		// detection at the merge output) atomically — see bulkload_indexed.go.
		return ks.bulkLoadIndexed(rows)
	}

	cfg := ks.builderCfg()
	b := newBulkBuilder(ks.tx.pgr, cfg)
	if err := ks.bulkLoadRows(rows, cfg, b, nil); err != nil {
		// mapBtreeErr translates btree.ErrKeyTooLarge (an oversize key
		// from bulkLeafEntry) to the public gmdb.ErrKeyTooLarge sentinel
		// and passes every other error (ErrKeyEmpty, ordering, I/O)
		// through unchanged.
		return 0, mapBtreeErr(err)
	}
	root, count, err := b.finish()
	if err != nil {
		return 0, err
	}
	// Retire any leftover root (a truly empty keyspace has Root == 0;
	// this is defense for an empty-but-allocated root) before publishing
	// the bulk-built tree. FreeSubtree(0) is a no-op.
	if _, err := btree.FreeSubtree(btreeWriter{ks.tx.pgr}, cfg, ks.desc.Root); err != nil {
		return 0, mapBtreeErr(err)
	}
	ks.desc.Root = root
	ks.desc.Count = count
	ks.markDirty()
	ks.markCursorsStale()
	return count, nil
}

// bulkLoadRows drives the row stream into the bulk builder b, adding one
// leaf entry per (key, value) pair, and — when onRow is non-nil — invoking
// it after each row entry is added. The indexed path (bulkLoadIndexed)
// uses onRow to feed per-index sorters from the same single pass over rows
// (the iter.Seq2 input may be a one-shot generator, so the row tree and the
// index entries must both be derived in one traversal). Returns the first
// error encountered (input order, empty key, oversized entry, or I/O).
func (ks *Keyspace) bulkLoadRows(rows iter.Seq2[[]byte, []byte], cfg page.Config, b *bulkBuilder, onRow func(key, value []byte) error) error {
	var loopErr error
	rows(func(key, value []byte) bool {
		if len(key) == 0 {
			loopErr = ErrKeyEmpty
			return false
		}
		e, err := bulkLeafEntry(ks.tx.pgr, cfg, key, value)
		if err != nil {
			loopErr = err
			return false
		}
		if err := b.add(e, key); err != nil {
			loopErr = err
			return false
		}
		if onRow != nil {
			if err := onRow(key, value); err != nil {
				loopErr = err
				return false
			}
		}
		return true
	})
	return loopErr
}

// BulkLoad replaces the contents of an empty SetKeyspace with the sorted
// stream produced by rows. Input MUST be in strictly-ascending (key, value)
// lex order — keys strictly ascending across groups, values strictly
// ascending within a key's group; a non-ascending key OR value returns
// ErrBulkLoadOutOfOrder. Duplicate (key, value) pairs are silently
// deduplicated. The keyspace must be empty (Count == 0), else
// ErrBulkLoadNonEmpty. Returns the number of distinct (key, value) members
// written. Per bulkload.md §API.
//
// Each key's value set is stored exactly as the per-Put path would: a
// subpage until adding a further value would push it past the 50%-of-leaf
// promotion threshold (the first value always stays a subpage, matching
// Put's genesis path), otherwise a nested B+tree (values as keys with
// empty values). Both the
// top-level tree and any nested value-trees are built bottom-up and
// pwritten directly (slab bypass). Memory is O(depth × pageSize): a key's
// values are buffered only up to the subpage threshold (≈ ½ page); a set
// that exceeds it is streamed into a nested builder, never fully
// materialised. Published only at Tx.Commit; a crash or rollback leaves
// the keyspace at its pre-BulkLoad state (bulkload.md §Atomicity).
//
// On an indexed SetKeyspace BulkLoad also builds every index's data tree:
// it runs each index's extractor per accepted (key, value) member, with the
// same external sort + atomic all-or-nothing publish as the Keyspace path
// (see bulkload_indexed.go). A unique-index duplicate aborts the whole load
// with ErrIndexUniqueViolation.
//
// Errors: ErrReadOnly, ErrKeyspaceClosed, ErrBulkLoadNonEmpty,
// ErrBulkLoadOutOfOrder, ErrKeyEmpty, ErrValueSizeMismatch (a value whose
// length differs from a non-zero FixedValueSize), ErrIndexUniqueViolation
// (duplicate key in a unique index), a wrapped I/O error on a sort-spill
// failure, and btree/I-O errors.
func (ks *SetKeyspace) BulkLoad(rows iter.Seq2[[]byte, []byte]) (uint64, error) {
	if err := ks.requireWritable(); err != nil {
		return 0, err
	}
	if ks.desc.Count != 0 {
		return 0, ErrBulkLoadNonEmpty
	}
	if len(ks.indexes) > 0 {
		return ks.bulkLoadIndexed(rows)
	}

	cfg := ks.builderCfg()
	sb := &setBulk{
		top:       newBulkBuilder(ks.tx.pgr, cfg),
		pw:        ks.tx.pgr,
		cfg:       cfg,
		fvs:       ks.desc.FixedValueSize,
		threshold: page.SubpagePromotionThreshold(cfg),
	}
	if err := ks.bulkLoadStream(rows, sb, nil); err != nil {
		return 0, mapBtreeErr(bulkMapEntryTooLarge(err))
	}
	if err := sb.flush(); err != nil {
		return 0, mapBtreeErr(bulkMapEntryTooLarge(err))
	}
	root, _, err := sb.top.finish()
	if err != nil {
		return 0, mapBtreeErr(err)
	}
	if _, err := btree.FreeSubtree(btreeWriter{ks.tx.pgr}, cfg, ks.desc.Root); err != nil {
		return 0, mapBtreeErr(err)
	}
	ks.desc.Root = root
	ks.desc.Count = sb.total
	ks.markDirty()
	ks.markSetCursorsStale()
	return sb.total, nil
}

// bulkLoadStream drives the (key, value) stream into the setBulk
// accumulator sb, applying the SetKeyspace input contract: keys strictly
// ascending across groups, values strictly ascending within a key's group,
// adjacent duplicate (key, value) pairs silently deduped, value-size matched
// against a non-zero FixedValueSize, nil value normalised to empty. When
// onMember is non-nil it fires once per accepted (non-duplicate) set member
// AFTER the member is added to sb — the indexed path uses it to feed the
// per-index sorters from the same single pass (the iter.Seq2 input may be a
// one-shot generator). Returns the first error encountered. The caller still
// performs the final sb.flush() + sb.top.finish().
func (ks *SetKeyspace) bulkLoadStream(rows iter.Seq2[[]byte, []byte], sb *setBulk, onMember func(setKey, setValue []byte) error) error {
	fvs := ks.desc.FixedValueSize
	var loopErr error
	rows(func(key, value []byte) bool {
		if len(key) == 0 {
			loopErr = ErrKeyEmpty
			return false
		}
		if fvs != 0 && len(value) != int(fvs) {
			loopErr = fmt.Errorf("%w: value len %d, keyspace FixedValueSize %d", ErrValueSizeMismatch, len(value), fvs)
			return false
		}
		if value == nil {
			value = []byte{}
		}
		// Key transition: flush the previous key's value set.
		if !sb.haveKey || !bytes.Equal(key, sb.curKey) {
			if sb.haveKey {
				if bytes.Compare(key, sb.curKey) <= 0 {
					loopErr = ErrBulkLoadOutOfOrder
					return false
				}
				if err := sb.flush(); err != nil {
					loopErr = err
					return false
				}
			}
			sb.startKey(key)
		}
		// Within the current key: dedup adjacent duplicates, enforce
		// strictly-ascending values.
		if sb.haveValue {
			switch bytes.Compare(value, sb.lastValue) {
			case 0:
				return true // duplicate (key, value): silently deduped
			case -1:
				loopErr = ErrBulkLoadOutOfOrder
				return false
			}
		}
		if err := sb.addValue(value); err != nil {
			loopErr = err
			return false
		}
		sb.setLastValue(value)
		if onMember != nil {
			// The member is accepted: (key, value) is a fresh distinct set
			// member. Index entries are derived from the same (setKey,
			// setValue) the row side just stored.
			if err := onMember(key, value); err != nil {
				loopErr = err
				return false
			}
		}
		return true
	})
	return loopErr
}

// setBulk accumulates one SetKeyspace key's value set during BulkLoad and
// emits the per-key storage (subpage or nested B+tree) into the top-level
// builder. Memory stays O(depth × pageSize): values are buffered only while
// the set fits the subpage threshold; once exceeded, the buffer is drained
// into a nested bulkBuilder and subsequent values stream straight through.
type setBulk struct {
	top       *bulkBuilder
	pw        bulkPageWriter
	cfg       page.Config
	fvs       uint16
	threshold int

	haveKey   bool
	curKey    []byte
	curKeyBuf []byte

	// Subpage-candidate buffer (used only until promotion).
	buffered     [][]byte
	bufferedSize int // SubpageHeaderSize + Σ entrySize

	haveValue    bool
	lastValue    []byte
	lastValueBuf []byte

	nested *bulkBuilder // non-nil once the set is promoted to a nested tree
	total  uint64       // total members across all keys
}

// startKey begins accumulating a fresh key's value set.
func (sb *setBulk) startKey(key []byte) {
	sb.curKeyBuf = append(sb.curKeyBuf[:0], key...)
	sb.curKey = sb.curKeyBuf
	sb.haveKey = true
	sb.buffered = sb.buffered[:0]
	sb.bufferedSize = page.SubpageHeaderSize
	sb.haveValue = false
	sb.nested = nil
}

// setLastValue records the most recent value for dedup / order checks.
func (sb *setBulk) setLastValue(v []byte) {
	sb.lastValueBuf = append(sb.lastValueBuf[:0], v...)
	sb.lastValue = sb.lastValueBuf
	sb.haveValue = true
}

// entrySize is one value's contribution to the subpage's DataSize.
func (sb *setBulk) entrySize(v []byte) int {
	if sb.fvs != 0 {
		return int(sb.fvs)
	}
	return 2 + len(v) // ValueLen uint16 + bytes
}

// addValue adds v to the current set (caller has validated size, dedup, and
// order). Buffers it as a subpage entry, or — once the subpage would exceed
// the promotion threshold — streams it into the nested builder.
func (sb *setBulk) addValue(v []byte) error {
	// Member bound (limits.md: set values are bounded by the maximum
	// key size — a member becomes a nested-tree KEY on promotion):
	// the same gate the online putIntoNestedTree applies via
	// btree.Put, so bulk and incremental loads accept identical
	// data. Checked before EITHER storage branch — the post-
	// promotion nested path must not readmit what the subpage path
	// rejects.
	if !setValueWithinInlineBound(sb.cfg, len(v)) {
		return btree.ErrKeyTooLarge
	}
	sb.total++
	if sb.nested != nil {
		return sb.nested.add(page.LeafEntry{Key: v}, v)
	}
	newSize := sb.bufferedSize + sb.entrySize(v)
	// Promote only when a SECOND-or-later value would push the subpage past
	// the threshold. The first value of a key always stays a subpage —
	// exactly as SetKeyspace.Put's genesis path, which never threshold-
	// checks the first value (set_keyspace.go Put: a new key's single value
	// is stored as a subpage regardless of size). This keeps the
	// bulk-built storage shape identical to the per-Put shape.
	if len(sb.buffered) > 0 && newSize > sb.threshold {
		if err := sb.promote(); err != nil {
			return err
		}
		return sb.nested.add(page.LeafEntry{Key: v}, v)
	}
	sb.buffered = append(sb.buffered, bytes.Clone(v))
	sb.bufferedSize = newSize
	return nil
}

// promote switches the current key from subpage buffering to a nested
// bulkBuilder, draining the already-buffered values into it.
func (sb *setBulk) promote() error {
	sb.nested = newBulkBuilder(sb.pw, sb.cfg)
	for _, v := range sb.buffered {
		if err := sb.nested.add(page.LeafEntry{Key: v}, v); err != nil {
			return err
		}
	}
	sb.buffered = sb.buffered[:0]
	sb.bufferedSize = page.SubpageHeaderSize
	return nil
}

// flush emits the current key's storage to the top-level builder: an
// AddNestedTreeRef cell if promoted, else an AddSubpage cell.
func (sb *setBulk) flush() error {
	if !sb.haveKey {
		return nil
	}
	// Set keys share the ordinary key contract (mirrors bulkLeafEntry
	// on the Keyspace path): the uint32 encoding bound gates, and an
	// over-threshold set key takes the overflow-key form with its
	// extent written through the bulk writer (page-formats.md
	// §Overflow-Key Cells).
	if !btree.KeyWithinBound(len(sb.curKey)) {
		return btree.ErrKeyTooLarge
	}
	e := page.LeafEntry{Key: sb.curKey}
	if t := sb.cfg.InlineThreshold(); len(sb.curKey) > t {
		ext, err := writeBulkOverflowChain(sb.top.pw, sb.cfg, sb.curKey[t:])
		if err != nil {
			return err
		}
		e.Flags |= page.CellFlagOverflowKey
		e.KeyTotalLen = uint32(len(sb.curKey))
		e.Key = sb.curKey[:t]
		e.KeyExtPage = ext
	}
	if sb.nested != nil {
		root, cnt, err := sb.nested.finish()
		if err != nil {
			return err
		}
		e.Flags |= page.CellFlagMultiValue | page.CellFlagNestedTree
		e.NestedRoot = root
		e.NestedCount = cnt
		return sb.top.add(e, sb.curKey)
	}
	sub, err := page.EncodeSubpage(sb.buffered, sb.fvs)
	if err != nil {
		return fmt.Errorf("gmdb: bulkload encode subpage: %w", err)
	}
	e.Flags |= page.CellFlagMultiValue
	e.Value = sub
	return sb.top.add(e, sb.curKey)
}

// bulkLeafEntry builds the leaf entry for one (key, value): an inline entry
// when it fits an empty leaf, otherwise a streamed overflow chain plus an
// overflow-reference entry. nil value is normalised to empty (the
// nil-value-as-empty invariant). The inline/overflow boundary is btree's
// (shared with Put) so a value Put would inline, BulkLoad inlines.
//
// Decoupled from any *Keyspace: it takes the overflow writer (the live
// pager for BulkLoad; a fresh-file writer for CopyTo's compacting rebuild)
// so both bottom-up builders share one overflow encoder.
func bulkLeafEntry(pw bulkOverflowWriter, cfg page.Config, key, value []byte) (page.LeafEntry, error) {
	if value == nil {
		value = []byte{}
	}
	// Storable-key bound first (limits.md §Maximum Key Size) — the
	// same spec-bound gate as btree.Put, so bulk and per-op loads
	// accept identical keys.
	if !btree.KeyWithinBound(len(key)) {
		return page.LeafEntry{}, btree.ErrKeyTooLarge
	}
	var e page.LeafEntry
	if !btree.NeedsOverflow(cfg, key, value) {
		e = page.LeafEntry{Key: key, Value: value}
	} else {
		firstID, err := writeBulkOverflowChain(pw, cfg, value)
		if err != nil {
			return page.LeafEntry{}, err
		}
		e = page.LeafEntry{
			Flags:        page.CellFlagOverflow,
			Key:          key,
			OverflowPage: firstID,
			TotalLen:     uint64(len(value)),
		}
	}
	// Key half: an over-threshold key takes the overflow-key form
	// (page-formats.md §Overflow-Key Cells) with its extent written
	// through the same bulk writer as value overflow.
	t := cfg.InlineThreshold()
	if len(key) > t {
		ext, err := writeBulkOverflowChain(pw, cfg, key[t:])
		if err != nil {
			return page.LeafEntry{}, err
		}
		e.Flags |= page.CellFlagOverflowKey
		e.KeyTotalLen = uint32(len(key))
		e.Key = key[:t]
		e.KeyExtPage = ext
	}
	return e, nil
}

// sharedLen returns the length of the longest common byte prefix of a
// and b — the bulk branch builder's running shared-prefix tally for
// segregated-branch sizing.
func sharedLen(a, b []byte) int {
	n := min(len(a), len(b))
	for i := range n {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}

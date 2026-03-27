// Package btree implements B+tree operations with copy-on-write semantics.
// It operates on a contiguous []byte page buffer (mmap or test allocation)
// with no I/O or OS dependencies. Mutations are tracked via CoW page sets
// and an allocation bitmap; the caller (transaction layer) handles commit.
package btree

import (
	"errors"

	"github.com/thegrumpylion/gmdb/internal/bitmap"
	"github.com/thegrumpylion/gmdb/internal/page"
)

// ErrNoSpace is returned when the bitmap has no free pages for allocation.
var ErrNoSpace = errors.New("btree: no free pages")

// ErrCursorStale is returned when cursor operations are attempted after the
// tree was mutated externally (via Put, Delete, or DeleteRange).
var ErrCursorStale = errors.New("btree: cursor stale after tree mutation")

// Config holds tree-level configuration set once at creation.
type Config struct {
	Page page.PageConfig

	// SplitBias controls the fill percentage for the packed side during
	// biased leaf splits. When sequential inserts are detected (append or
	// prepend), the split is biased so the "done" side is filled to this
	// percentage. Range: 50–100. Values ≤ 50 disable biasing (always
	// 50/50). 0 is treated as the default (90).
	SplitBias int
}

// normalize returns a copy with defaults applied and values clamped.
func (c Config) normalize() Config {
	if c.SplitBias == 0 {
		c.SplitBias = 90
	}
	if c.SplitBias < 50 {
		c.SplitBias = 50
	}
	if c.SplitBias > 100 {
		c.SplitBias = 100
	}
	return c
}

// Tree manages a B+tree over a contiguous page buffer.
type Tree struct {
	data []byte         // page buffer (mmap or test)
	cfg  Config         // tree + page config
	bm   *bitmap.Bitmap // allocation bitmap

	root    uint64              // current root page ID (0 = empty tree)
	cow     map[uint64]struct{} // pages CoW'd in this transaction
	retired []uint64            // pages from previous txns to retire via RPL
	gen     uint64              // mutation generation counter for cursor staleness
	scratch []byte              // reusable page-sized buffer for split/merge probes
}

// New creates a Tree. root=0 means empty tree.
func New(data []byte, cfg Config, bm *bitmap.Bitmap, root uint64) *Tree {
	cfg = cfg.normalize()
	return &Tree{
		data:    data,
		cfg:     cfg,
		bm:      bm,
		root:    root,
		cow:     make(map[uint64]struct{}),
		scratch: make([]byte, cfg.Page.PageSize),
	}
}

// Root returns the current root page ID (0 = empty tree).
func (t *Tree) Root() uint64 {
	return t.root
}

// Retired returns pages from previous transactions that were replaced by CoW.
// These must be appended to the RPL at commit time.
func (t *Tree) Retired() []uint64 {
	return t.retired
}

// CowPages returns the set of pages CoW'd in this transaction.
func (t *Tree) CowPages() map[uint64]struct{} {
	return t.cow
}

// Reset clears all CoW tracking state for a new transaction.
func (t *Tree) Reset(root uint64) {
	t.root = root
	clear(t.cow)
	t.retired = t.retired[:0]
	t.gen++
}

// pageSlice returns the page-sized buffer for the given page ID.
func (t *Tree) pageSlice(pageID uint64) []byte {
	off := pageID * uint64(t.cfg.Page.PageSize)
	return t.data[off : off+uint64(t.cfg.Page.PageSize)]
}

// allocPage allocates a fresh zeroed page from the bitmap and adds it
// to the CoW set. Returns the page ID.
func (t *Tree) allocPage() (uint64, error) {
	pageID, ok := t.bm.FindFirstFree()
	if !ok {
		return 0, ErrNoSpace
	}
	buf := t.pageSlice(pageID)
	clear(buf)
	t.cow[pageID] = struct{}{}
	return pageID, nil
}

// cowPage ensures the page has a private copy for this transaction.
// If already CoW'd, returns the same page ID. Otherwise, allocates a new
// page, copies the content, and retires the old page.
func (t *Tree) cowPage(pageID uint64) (uint64, error) {
	if _, ok := t.cow[pageID]; ok {
		return pageID, nil
	}
	newID, ok := t.bm.FindFirstFree()
	if !ok {
		return 0, ErrNoSpace
	}
	copy(t.pageSlice(newID), t.pageSlice(pageID))
	t.cow[newID] = struct{}{}
	t.retired = append(t.retired, pageID)
	return newID, nil
}

// cowPageFresh is like cowPage but skips copying the page content. The caller
// must fully rebuild the new page (via rebuildLeaf/rebuildBranch). Returns the
// new page ID and whether a fresh page was allocated (true) or the page was
// already CoW'd (false). When fresh is true, the original pageID is retired
// but its buffer remains readable for the duration of this transaction.
func (t *Tree) cowPageFresh(pageID uint64) (newID uint64, fresh bool, err error) {
	if _, ok := t.cow[pageID]; ok {
		return pageID, false, nil
	}
	id, ok := t.bm.FindFirstFree()
	if !ok {
		return 0, false, ErrNoSpace
	}
	t.cow[id] = struct{}{}
	t.retired = append(t.retired, pageID)
	return id, true, nil
}

// freePage releases a page that was allocated or CoW'd in this transaction.
// The page is returned to the bitmap as a loose page. Panics if the page
// was not CoW'd in this transaction — the caller must never free a page
// from a previous transaction without going through the RPL.
func (t *Tree) freePage(pageID uint64) {
	if _, ok := t.cow[pageID]; !ok {
		panic("btree: freePage called on non-CoW'd page")
	}
	t.bm.Set(pageID)
	delete(t.cow, pageID)
}

// retirePage adds a page to the retired list if it's from a previous
// transaction, or frees it immediately if it was CoW'd in this transaction.
// Used by subtree retirement and overflow chain cleanup where pages may
// come from either the current or previous transactions.
func (t *Tree) retirePage(pageID uint64) {
	if _, ok := t.cow[pageID]; ok {
		t.bm.Set(pageID)
		delete(t.cow, pageID)
	} else {
		t.retired = append(t.retired, pageID)
	}
}

// mutated increments the generation counter. Called after every tree mutation.
func (t *Tree) mutated() {
	t.gen++
}

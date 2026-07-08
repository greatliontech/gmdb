package btree

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/thegrumpylion/gmdb/internal/page"
)

// ErrCursorUnpositioned is returned by Cursor.Err when the cursor
// is in the Unpositioned state — i.e. it was created or Reset and
// no successful navigation has occurred yet. Distinct from
// end-of-iteration (Err == nil) per transactions.md §Cursor State
// Machine.
var ErrCursorUnpositioned = errors.New("btree: cursor not positioned")

// ErrReadOnly is returned by Cursor.Delete when invoked on a
// read-only cursor (constructed via NewReadCursor or on a tx /
// keyspace without write privileges). The public-API layer
// maps this to gmdb.ErrReadOnly.
var ErrReadOnly = errors.New("btree: cursor is read-only")

// ErrCursorStale is the sentinel returned by Cursor's non-
// repositioning methods (Next / Prev / Current / Delete) when an
// external mutation has bumped the cursor's generation counter
// since the cursor was last positioned. The caller's expected
// response is to repositioning (First / Last / Seek / SeekGE) or
// to call Cursor.Delete (which clears stale state by re-Seeking
// past its own mutation per transactions.md §Cursor.Delete
// post-delete state).
//
// Currently only Cursor.Delete is a mutation source visible
// to the cursor, and it self-clears the stale flag before
// returning — so this sentinel is unreachable from internal use.
// The mechanism is scaffolding for keyspace integration,
// where sibling cursors and keyspace.Put/Delete will bump gen
// externally and surface this sentinel.
var ErrCursorStale = errors.New("btree: cursor invalidated by external mutation")

// cursorState enumerates the three states from transactions.md
// §Cursor State Machine.
type cursorState uint8

const (
	csUnpositioned cursorState = iota
	csPositioned
	csEndOfIteration
)

// Cursor is a bidirectional cursor over a btree subtree rooted at
// some pageID. Implements the state machine from transactions.md
// §Cursor State Machine: Unpositioned → {Positioned,
// End-of-iteration} via First / Last / Seek / SeekGE; Positioned
// → {Positioned, End-of-iteration} via Next / Prev / Delete.
//
// Slice ownership. The (key, value) byte slices returned by
// Current / First / Last / Next / Prev / Seek / SeekGE are
// borrowed for inline entries:
//   - Value aliases the leaf page buffer; valid until the next
//     cursor operation that crosses a leaf transition (a Next /
//     Prev / Delete that moves to a different leaf), or the
//     enclosing transaction closes.
//   - Key for compressed-leaf delta entries aliases the cursor's
//     internal keyBuf (the LeafIter reconstruction buffer per
//     page-formats.md §Cursor Iteration); valid until the next
//     cursor movement. Key for restart entries and uncompressed
//     leaves aliases the leaf page buffer with the same lifetime
//     as Value.
//
// Overflow-entry values diverge from the borrowed-reference rule:
// the value is assembled from a 1+N-page contiguous chain that
// has header/footer gaps between value bytes, so a single
// contiguous mmap slice can't span it. The cursor eagerly
// assembles into a heap-allocated slice on every step that lands
// on an overflow entry; the returned Value is caller-owned with
// caller-controlled lifetime. (Future: lazy-on-Current assembly
// + a tx-scoped arena could amortize.)
//
// Callers that need to retain a returned key past the next cursor
// op must bytes.Clone before moving the cursor — per api-surface.md
// §Byte-slice ownership.
//
// Zero-allocation steady state. The keyBuf / bufKeys / bufEnts
// scratch buffers are reused across leaf transitions: the cursor
// pulls them back from each LeafIter via KeyBuf / BufKeys /
// BufEnts and threads them into the next leaf's iter, growing
// only when a leaf's restart group exceeds prior bounds.
type Cursor struct {
	pw  PageWriter // nil for read-only cursors
	pr  PageReader // never nil
	cfg page.Config

	rootID         uint64
	mergeThreshold uint8 // consulted only by Cursor.Delete

	state cursorState
	err   error // sticky; cleared on successful re-positioning

	// Generation counter. Bumped by Cursor.Delete (a self-
	// mutation, since the cursor invoked it) and by any future
	// external mutator that calls c.MarkStale. `posGen` records
	// the gen value at the cursor's most recent successful
	// positioning; a mismatch at non-positioning methods surfaces
	// as ErrCursorStale per transactions.md.
	gen    uint64
	posGen uint64

	// Descent path. path[0] is the root; path[len-1] is the leaf
	// frame whose pageID is the current leaf. For an empty tree
	// (rootID == 0) or before any positioning, path is empty.
	path []pathFrame
	iter page.LeafIter

	// Scratch buffers reused across leaf transitions.
	keyBuf  []byte
	bufKeys []byte
	bufEnts []page.LeafEntry

	// Current entry — borrowed slices from the active leaf's iter
	// / page buffer. Refreshed on every successful navigation.
	curKey   []byte
	curValue []byte

	// curKeyBuf holds an independently-allocated copy of curKey
	// when the natural curKey source is NOT one of the slice-
	// ownership categories spec'd by api-surface.md §Byte Slice
	// Ownership (mmap / slab / cursor keyBuf). The Seek* exact-
	// match path is the canonical case: SearchLeafIter returns
	// entry.Key == nil for exact hits (the caller owns target),
	// and a raw `c.curKey = target` aliases caller storage —
	// outside the spec's enumerated lifetime sources. Stashing
	// via `append(c.curKeyBuf[:0], target...)` keeps the cursor
	// surface uniform.
	curKeyBuf []byte
}

// NewReadCursor returns a read-only cursor over the tree rooted at
// rootID. Delete returns ErrReadOnly on a cursor constructed this
// way. The cursor is initially Unpositioned per the state machine.
func NewReadCursor(pr PageReader, cfg page.Config, rootID uint64) *Cursor {
	return &Cursor{pr: pr, cfg: cfg, rootID: rootID, state: csUnpositioned}
}

// NewCursor returns a writable cursor. Cursor.Delete uses the
// provided mergeThreshold for the underlying btree.Delete call;
// caller threads through the keyspace's Options.MergeThreshold.
//
// pw must be non-nil; pw is used both as PageWriter (for Delete)
// and as PageReader (for navigation). A nil pw yields a panic on
// first navigation, not on construction — pin via tests, not by
// argument validation here.
func NewCursor(pw PageWriter, cfg page.Config, rootID uint64, mergeThreshold uint8) *Cursor {
	return &Cursor{pw: pw, pr: pw, cfg: cfg, rootID: rootID, mergeThreshold: mergeThreshold, state: csUnpositioned}
}

// RootID returns the current rootID. Cursor.Delete updates this in
// place — the keyspace layer reads it post-Delete to refresh its
// descriptor.
func (c *Cursor) RootID() uint64 { return c.rootID }

// Err returns the sticky error explaining the current state.
// Returns ErrCursorUnpositioned for Unpositioned; nil for
// Positioned and End-of-iteration; the stale or external error
// otherwise. See transactions.md §Cursor State Machine for the
// Unpositioned-vs-End-of-iteration distinction.
func (c *Cursor) Err() error {
	if c.err != nil {
		return c.err
	}
	if c.state == csUnpositioned {
		return ErrCursorUnpositioned
	}
	return nil
}

// Current returns the cursor's current (key, value), or (nil, nil)
// if the cursor is Unpositioned or End-of-iteration. The returned
// slices are borrowed per the type doc's slice-ownership rules.
func (c *Cursor) Current() (key, value []byte) {
	if c.state != csPositioned {
		return nil, nil
	}
	if c.gen != c.posGen {
		c.err = ErrCursorStale
		return nil, nil
	}
	return c.curKey, c.curValue
}

// First positions the cursor at the leftmost leaf entry. Returns
// (nil, nil) on an empty tree (rootID == 0) with state =
// End-of-iteration; otherwise (key, value) of the first entry and
// state = Positioned.
func (c *Cursor) First() (key, value []byte) {
	c.err = nil
	if c.rootID == 0 {
		c.resetPath()
		c.transitionToEnd()
		return nil, nil
	}
	if err := c.descendLeftmost(c.rootID); err != nil {
		c.err = err
		return nil, nil
	}
	return c.firstInLeaf()
}

// Last positions the cursor at the rightmost leaf entry.
func (c *Cursor) Last() (key, value []byte) {
	c.err = nil
	if c.rootID == 0 {
		c.resetPath()
		c.transitionToEnd()
		return nil, nil
	}
	if err := c.descendRightmost(c.rootID); err != nil {
		c.err = err
		return nil, nil
	}
	return c.lastInLeaf()
}

// Seek positions the cursor at the entry with exactly the given
// key. On a miss returns (nil, nil) with state = End-of-iteration
// and Err == nil. (Use SeekGE if a successor on miss is desired.)
func (c *Cursor) Seek(target []byte) (key, value []byte) {
	c.err = nil
	if c.rootID == 0 {
		c.resetPath()
		c.transitionToEnd()
		return nil, nil
	}
	_, entry, found, err := c.descendToKey(c.rootID, target)
	if err != nil {
		c.err = err
		return nil, nil
	}
	if !found {
		c.transitionToEnd()
		return nil, nil
	}
	c.posGen = c.gen
	c.state = csPositioned
	c.adoptTargetKey(target)
	c.curValue = c.valueFor(entry)
	return c.curKey, c.curValue
}

// SeekGE positions the cursor at the smallest key ≥ target. On a
// past-end (every key strictly less than target) returns (nil, nil)
// with state = End-of-iteration and Err == nil.
func (c *Cursor) SeekGE(target []byte) (key, value []byte) {
	c.err = nil
	if c.rootID == 0 {
		c.resetPath()
		c.transitionToEnd()
		return nil, nil
	}
	idx, entry, found, err := c.descendToKey(c.rootID, target)
	if err != nil {
		c.err = err
		return nil, nil
	}
	if found {
		c.posGen = c.gen
		c.state = csPositioned
		c.adoptTargetKey(target)
		c.curValue = c.valueFor(entry)
		return c.curKey, c.curValue
	}
	// Miss: idx is the in-leaf successor index. If idx < the
	// iter's count, the successor lives in this leaf and
	// SearchLeafIter has already returned the successor's entry
	// in `entry`. Otherwise advance to the next leaf.
	if idx < c.iter.Count() {
		c.posGen = c.gen
		c.state = csPositioned
		c.adoptEntry(entry)
		return c.curKey, c.curValue
	}
	// Past this leaf — try the next leaf in document order.
	if !c.advanceToNextLeaf() {
		c.transitionToEnd()
		return nil, nil
	}
	return c.firstInLeaf()
}

// Next advances the cursor by one entry. Behavior per state:
//   - Unpositioned: behaves like First.
//   - Positioned: advances to the next entry; transitions to
//     End-of-iteration when past the last entry.
//   - End-of-iteration: returns (nil, nil); stays End-of-iteration.
func (c *Cursor) Next() (key, value []byte) {
	if c.state == csUnpositioned {
		return c.First()
	}
	if c.state == csEndOfIteration {
		return nil, nil
	}
	if c.gen != c.posGen {
		c.err = ErrCursorStale
		return nil, nil
	}
	e, ok := c.iter.Next()
	if ok {
		c.adoptEntry(e)
		return c.curKey, c.curValue
	}
	if !c.advanceToNextLeaf() {
		c.transitionToEnd()
		return nil, nil
	}
	return c.firstInLeaf()
}

// Prev steps the cursor backward by one entry. Behavior per state:
//   - Unpositioned: behaves like Last.
//   - Positioned: steps to the prior entry; End-of-iteration when
//     before the first entry.
//   - End-of-iteration: returns (nil, nil); stays End-of-iteration.
func (c *Cursor) Prev() (key, value []byte) {
	if c.state == csUnpositioned {
		return c.Last()
	}
	if c.state == csEndOfIteration {
		return nil, nil
	}
	if c.gen != c.posGen {
		c.err = ErrCursorStale
		return nil, nil
	}
	e, ok := c.iter.Prev()
	if ok {
		c.adoptEntry(e)
		return c.curKey, c.curValue
	}
	if !c.advanceToPrevLeaf() {
		c.transitionToEnd()
		return nil, nil
	}
	return c.lastInLeaf()
}

// Delete removes the current entry. The cursor must be Positioned
// (transactions.md §Cursor.Delete post-delete state) — otherwise
// returns ErrCursorUnpositioned. After a successful delete the
// cursor advances to the post-delete successor (the entry that
// followed the deleted entry); if no such entry exists the cursor
// transitions to End-of-iteration.
//
// Delete tolerates CoW + merge cascade triggered by its own
// btree.Delete call — the cursor's path is fully rebuilt via the
// internal SeekGE re-position past the deleted key. The pre-
// delete leaf may be freed mid-operation, yet the post-Delete
// Next / Prev / Current return structurally-correct entries.
//
// External staleness. If an EXTERNAL mutator has bumped the
// cursor's gen since the cursor was last positioned, Delete
// refuses with ErrCursorStale — symmetric with Next / Prev /
// Current. The caller must re-position via First / Last / Seek /
// SeekGE before retrying. (curKey may alias storage the external
// mutation has freed; proceeding with bytes.Clone(c.curKey) could
// stage a delete against garbage. Within this package only
// Cursor.Delete itself bumps gen and it self-re-Seeks, so the
// path is unreachable from internal use; the keyspace layer's
// MarkStale calls exercise it.)
//
// Per the spec the successor returned by the internal SeekGE is
// strictly greater than deletedKey (deletedKey is gone), so the
// SeekGE exact-match branch cannot fire — curKey is set from the
// successor's iter-returned entry, not from the deletedKey
// argument.
func (c *Cursor) Delete() error {
	if c.pw == nil {
		return ErrReadOnly
	}
	if c.state != csPositioned {
		return ErrCursorUnpositioned
	}
	if c.gen != c.posGen {
		c.err = ErrCursorStale
		return ErrCursorStale
	}
	// Capture the deleted key independent of any iter / page
	// buffer the upcoming btree.Delete will CoW-then-free. The
	// curKey slice may alias keyBuf which the re-SeekGE clobbers.
	deletedKey := bytes.Clone(c.curKey)

	newRoot, err := Delete(c.pw, c.cfg, c.rootID, c.mergeThreshold, deletedKey)
	if err != nil {
		// btree.Delete should not return ErrNotFound on a
		// Positioned cursor whose curKey was just decoded from
		// the live leaf — a mismatch indicates structural
		// corruption or a concurrency violation that bumped the
		// tree's state between cursor positioning and delete.
		return fmt.Errorf("btree: cursor.Delete underlying btree.Delete failed: %w", err)
	}
	c.rootID = newRoot
	c.gen++
	c.SeekGE(deletedKey)
	return nil
}

// MarkStale bumps the cursor's generation counter, causing the
// next non-repositioning op (Next / Prev / Current / Delete) to
// return / surface ErrCursorStale. The keyspace layer
// calls this on cursors whose state may have been invalidated by
// a sibling mutator (keyspace.Put / Delete, or a sibling cursor's
// Delete). Also clears curKey / curValue / iter so a caller that
// bypasses the gen check (e.g. a profiling hook or debug accessor
// reading c.curKey directly) sees nil rather than dangling
// references to potentially-freed leaf-buffer slices.
func (c *Cursor) MarkStale() {
	c.gen++
	c.curKey, c.curValue = nil, nil
	c.iter = page.LeafIter{}
}

// SetRootID updates the cursor's tracked root after a sibling
// mutation moved the keyspace's root via CoW. Called by the
// Keyspace.markCursorsStale path immediately after
// MarkStale so a caller re-positioning via First / Last / Seek /
// SeekGE descends from the live root rather than the (now-retired)
// pre-mutation root.
//
// A pre-existing latent bug
// captured `rootID` at construction and never refreshed
// it on sibling mutations. Re-positioning a stale cursor would
// descend from a FreePage'd root whose mmap-resident bytes survive
// only until the loose-pool reuses the id, producing either stale
// or corrupted reads.
func (c *Cursor) SetRootID(rootID uint64) {
	c.rootID = rootID
}

// resetPath releases the active iter's scratch buffers back to
// the cursor so the next leaf transition can re-thread them, then
// clears the descent path.
func (c *Cursor) resetPath() {
	c.reclaimIterBuffers()
	c.path = c.path[:0]
	c.curKey, c.curValue = nil, nil
}

// transitionToEnd moves the cursor to End-of-iteration with the
// invariant that quiescent states have `posGen == gen`. This pins
// "End-of-iteration is a normal terminal state, not a stale
// state" — a subsequent op should see (nil, nil) without
// ErrCursorStale even if external mutators bump gen between this
// transition and the next call. Used at every End-of-iteration
// entry point.
func (c *Cursor) transitionToEnd() {
	c.state = csEndOfIteration
	c.posGen = c.gen
	c.curKey, c.curValue = nil, nil
}

// reclaimIterBuffers pulls the active iter's keyBuf / bufKeys /
// bufEnts back into the cursor so the next leaf transition reuses
// them with zero allocation.
func (c *Cursor) reclaimIterBuffers() {
	c.keyBuf = c.iter.KeyBuf()
	c.bufKeys = c.iter.BufKeys()
	c.bufEnts = c.iter.BufEnts()
}

// adoptEntry copies the iter's last-decoded (key, value) pair into
// the cursor's cur* slots. For compressed-leaf delta entries the
// returned Key aliases keyBuf which the next iter call may
// clobber; the api-surface.md contract permits this (Key valid
// until next cursor op).
//
// Overflow entries: the value is eagerly assembled from the
// overflow chain into a heap-allocated slice. Heap ownership
// diverges from the inline-value mmap-borrow rule; see Cursor
// type doc for the full lifetime contract. On assembly error the
// cursor's err is set and curValue is left nil — state stays
// csPositioned (the key was found), but Err() surfaces the
// failure so the caller can decide whether to continue.
func (c *Cursor) adoptEntry(e page.LeafEntry) {
	c.curKey = e.Key
	if c.curKey == nil {
		c.curKey = emptyPositionedKey // see adoptTargetKey — nil is the miss channel
	}
	c.curValue = c.valueFor(e)
}

// valueFor returns the value bytes for entry e: e.Value verbatim
// for inline entries; the heap-assembled chain bytes for overflow
// entries. Records assembly errors via c.err and returns nil in
// that case.
func (c *Cursor) valueFor(e page.LeafEntry) []byte {
	if !e.IsOverflow() {
		return e.Value
	}
	val, err := readOverflowValue(c.pr, c.cfg, e)
	if err != nil {
		c.err = err
		return nil
	}
	return val
}

// adoptTargetKey stashes the caller's `target` into the cursor's
// private curKeyBuf, then sets curKey to that copy. Used on Seek*
// exact-match paths where SearchLeafIter returns entry.Key == nil
// (caller owns target) — a raw `c.curKey = target` would alias
// caller storage, which api-surface.md §Byte Slice Ownership does
// NOT enumerate as a permitted source. The `append(buf[:0], ...)`
// reuses the backing array across cursor ops so steady-state
// allocation pressure is zero.
func (c *Cursor) adoptTargetKey(target []byte) {
	c.curKeyBuf = append(c.curKeyBuf[:0], target...)
	c.curKey = c.curKeyBuf
	if c.curKey == nil {
		// A hit on the EMPTY key (legitimate in nested member
		// trees): nil is the cursor's miss/unpositioned channel, so
		// a positioned curKey must never be nil — a fresh cursor's
		// nil curKeyBuf would otherwise collapse the hit into a
		// reported miss.
		c.curKey = emptyPositionedKey
	}
}

// emptyPositionedKey is the non-nil zero-length key a positioned
// cursor reports for the empty stored key. Shared and immutable
// (len 0 — no writes can go through it).
var emptyPositionedKey = []byte{}

// branchPick chooses the child to follow at one branch level of a
// cursor descent. It receives the validated branch buffer and returns
// the frame's childIdx, the child page id, and a short label for the
// null-child corruption message. The three policies — leftmost,
// rightmost, key-search — are the only variation across the cursor's
// descent paths; everything else (page read, type check, validation,
// frame push, depth bound) is descendFrom's one skeleton.
type branchPick func(buf []byte) (childIdx uint16, child uint64, label string)

func pickLeftmost(buf []byte) (uint16, uint64, string) {
	return 0, page.BranchLeftmostChild(buf), "leftmost"
}

// descendFrom appends path frames from cur down to a leaf, choosing
// each branch's child via pick and finishing with onLeaf on the
// validated leaf reader (which initializes c.iter per the caller's
// positioning policy). Does NOT reset existing frames — the
// leaf-transition callers extend a partial path; root-level descents
// call resetPath first.
func (c *Cursor) descendFrom(cur uint64, pick branchPick, onLeaf func(r page.LeafReader)) error {
	for depth := 0; depth <= MaxTreeDepth; depth++ {
		buf, err := c.pr.Page(cur)
		if err != nil {
			return err
		}
		typ, _, _, _ := page.ReadHeader(buf)
		if page.IsLeafType(typ) {
			r := page.NewLeafReader(buf, c.cfg)
			if err := r.Validate(); err != nil {
				return fmt.Errorf("%w: leaf %d: %w", ErrCorrupted, cur, err)
			}
			c.path = append(c.path, pathFrame{pageID: cur})
			onLeaf(r)
			return nil
		}
		if typ != page.TypeBranch {
			return fmt.Errorf("%w: page %d unexpected type %d in cursor descent", ErrCorrupted, cur, typ)
		}
		if err := validateBranchPage(buf, c.cfg, cur); err != nil {
			return err
		}
		idx, child, label := pick(buf)
		if child == 0 {
			return fmt.Errorf("%w: null %s child in branch %d (index %d)", ErrCorrupted, label, cur, idx)
		}
		c.path = append(c.path, pathFrame{pageID: cur, childIdx: idx})
		cur = child
	}
	return ErrTreeTooDeep
}

func pickRightmost(cfg page.Config) branchPick {
	return func(buf []byte) (uint16, uint64, string) {
		n := page.BranchCellCount(buf)
		if n == 0 {
			return n, page.BranchLeftmostChild(buf), "rightmost"
		}
		return n, page.BranchCellAt(buf, cfg, n-1).Child, "rightmost"
	}
}

// descendLeftmost walks from rootID down through leftmost child
// pointers, populating c.path with branch frames (childIdx = 0
// each) and a leaf frame. Initializes the leaf iter for forward
// streaming. Returns ErrCorrupted on structural faults.
func (c *Cursor) descendLeftmost(rootID uint64) error {
	c.resetPath()
	return c.descendLeftmostFrom(rootID)
}

// descendRightmost walks rightmost-child pointers. The leaf iter
// is positioned past the end (idx == count) so Prev() returns the
// last entry.
func (c *Cursor) descendRightmost(rootID uint64) error {
	c.resetPath()
	return c.descendRightmostFrom(rootID)
}

// descendToKey walks rootID toward `target`, populating the path
// with each branch frame's chosen childIdx. On the leaf, performs
// SearchLeafIter to return both the in-leaf hit/insertion index
// AND a LeafIter positioned past the returned entry — so a
// subsequent Next() on the iter returns the entry immediately
// after the result, the cursor-friendly form per page-formats.md
// §Leaf Lookup.
func (c *Cursor) descendToKey(rootID uint64, target []byte) (idx int, entry page.LeafEntry, found bool, err error) {
	c.resetPath()
	err = c.descendFrom(rootID, func(buf []byte) (uint16, uint64, string) {
		i := page.BranchSearch(buf, c.cfg, target)
		return i, page.BranchChildAt(buf, c.cfg, i), "searched"
	}, func(r page.LeafReader) {
		idx, entry, found, c.iter = r.SearchLeafIter(target, c.keyBuf, c.bufKeys, c.bufEnts)
	})
	if err != nil {
		return 0, page.LeafEntry{}, false, err
	}
	return idx, entry, found, nil
}

// firstInLeaf calls iter.Next() to position at the first entry of
// the current leaf. Used after descendLeftmost or after a
// next-leaf transition.
func (c *Cursor) firstInLeaf() (key, value []byte) {
	e, ok := c.iter.Next()
	if !ok {
		// Empty leaf — unreachable in a well-formed tree (only
		// the root may be empty, and that's caught by the
		// rootID == 0 short-circuit in First). Treat as
		// end-of-iteration rather than panic.
		c.transitionToEnd()
		return nil, nil
	}
	c.posGen = c.gen
	c.state = csPositioned
	c.adoptEntry(e)
	return c.curKey, c.curValue
}

// lastInLeaf positions at the last entry of the current leaf via
// iter.At(count-1). The iter was constructed past-end (idx ==
// count); At(count-1) returns the entry at count-1 and sets idx
// to count, so subsequent Prev() walks backward by one entry per
// call per LeafIter's Next-then-Prev semantic. Direct iter.Prev()
// from past-end would return entry count-2 (skipping count-1),
// which is wrong for the cursor's "position at last entry"
// contract — see page/leaf_iter.go LeafIter.Prev's documented
// "step back from just-Nexted" semantic.
func (c *Cursor) lastInLeaf() (key, value []byte) {
	count := c.iter.Count()
	if count == 0 {
		// Empty leaf — unreachable in a well-formed tree (only
		// the root leaf may be empty, and that's caught by the
		// rootID == 0 short-circuit in Last).
		c.transitionToEnd()
		return nil, nil
	}
	e, ok := c.iter.At(count - 1)
	if !ok {
		c.transitionToEnd()
		return nil, nil
	}
	c.posGen = c.gen
	c.state = csPositioned
	c.adoptEntry(e)
	return c.curKey, c.curValue
}

// advanceToNextLeaf walks up the path looking for a branch frame
// whose childIdx is not already pointing at the rightmost child.
// On finding one, increments childIdx and re-descends to the
// leftmost leaf of that subtree, replacing the path frames below
// the pivot. Reclaims and re-threads the iter scratch buffers
// across the transition.
//
// Returns false if no further leaf exists (cursor was on the
// rightmost leaf), leaving the path intact for the caller's
// end-of-iteration transition.
func (c *Cursor) advanceToNextLeaf() bool {
	c.reclaimIterBuffers()
	// Drop the leaf frame; we're moving to a different leaf.
	c.path = c.path[:len(c.path)-1]
	for len(c.path) > 0 {
		top := &c.path[len(c.path)-1]
		buf, err := c.pr.Page(top.pageID)
		if err != nil {
			c.err = err
			return false
		}
		if err := validateBranchPage(buf, c.cfg, top.pageID); err != nil {
			c.err = err
			return false
		}
		n := page.BranchCellCount(buf)
		if int(top.childIdx) < int(n) {
			// Advance into the next sibling subtree at this
			// level, then descend leftmost to the leaf.
			top.childIdx++
			child := page.BranchChildAt(buf, c.cfg, top.childIdx)
			if child == 0 {
				c.err = fmt.Errorf("%w: null sibling child in branch %d at idx %d", ErrCorrupted, top.pageID, top.childIdx)
				return false
			}
			if err := c.descendLeftmostFrom(child); err != nil {
				c.err = err
				return false
			}
			return true
		}
		// This branch is exhausted; ascend.
		c.path = c.path[:len(c.path)-1]
	}
	return false
}

// advanceToPrevLeaf is the symmetric of advanceToNextLeaf. Walks
// up looking for a frame with childIdx > 0; decrements and
// descends rightmost.
func (c *Cursor) advanceToPrevLeaf() bool {
	c.reclaimIterBuffers()
	c.path = c.path[:len(c.path)-1]
	for len(c.path) > 0 {
		top := &c.path[len(c.path)-1]
		if top.childIdx == 0 {
			c.path = c.path[:len(c.path)-1]
			continue
		}
		top.childIdx--
		buf, err := c.pr.Page(top.pageID)
		if err != nil {
			c.err = err
			return false
		}
		if err := validateBranchPage(buf, c.cfg, top.pageID); err != nil {
			c.err = err
			return false
		}
		child := page.BranchChildAt(buf, c.cfg, top.childIdx)
		if child == 0 {
			c.err = fmt.Errorf("%w: null sibling child in branch %d at idx %d", ErrCorrupted, top.pageID, top.childIdx)
			return false
		}
		if err := c.descendRightmostFrom(child); err != nil {
			c.err = err
			return false
		}
		return true
	}
	return false
}

// descendLeftmostFrom appends path frames from `cur` downward,
// always taking the leftmost child. The leaf iter is initialized
// for forward streaming. Used by advanceToNextLeaf — does NOT
// reset the existing path frames above the descent root.
func (c *Cursor) descendLeftmostFrom(cur uint64) error {
	return c.descendFrom(cur, pickLeftmost, func(r page.LeafReader) {
		c.iter = r.IterForReuse(c.keyBuf, c.bufKeys, c.bufEnts)
	})
}

// descendRightmostFrom is the symmetric helper for
// advanceToPrevLeaf.
func (c *Cursor) descendRightmostFrom(cur uint64) error {
	return c.descendFrom(cur, pickRightmost(c.cfg), func(r page.LeafReader) {
		c.iter = r.IterAtForReuse(r.Count(), c.keyBuf, c.bufKeys, c.bufEnts)
	})
}

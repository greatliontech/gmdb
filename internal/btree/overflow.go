package btree

import (
	"bytes"
	"fmt"

	"github.com/greatliontech/gmdb/internal/page"
)

// Overflow value support per page-formats.md §Overflow Page and
// limits.md §Maximum Value Size. The btree promotes a value to an
// overflow chain when its inline encoding cannot fit on a single-
// entry leaf page. The chain is a contiguous run of 1 + N pages
// (first carries the page header with AdditionalPages = N; N
// followers carry pure value bytes).
//
// Lifecycle. Every overflow chain is owned by exactly one leaf
// entry; the chain is allocated when the leaf entry is created and
// freed (FreeRun in the same write tx) when the leaf entry is
// replaced with a new value or deleted. The btree's invariant:
// every bitmap-allocated overflow chain is reachable from a live
// leaf entry or in the write tx's pending-free set — no orphan
// chains.

// inlineEntryHeaderOverhead is the per-entry byte overhead in an
// uncompressed leaf, restart entry, or delta entry that uses the
// inline form: 1 byte CellFlags + 2 byte KeyLen + 4 byte ValueLen.
// (Delta entries replace KeyLen with SharedLen+UnsharedLen — same
// 2-byte width.) Used by needsOverflow to predict the inline entry
// size before constructing it.
const inlineEntryHeaderOverhead = 1 + 2 + 4

// overflowEntryHeaderOverhead is the per-entry byte overhead for an
// overflow-reference entry: 1 + 2 + 8 + 8 (CellFlags + KeyLen +
// OvflPage + TotalLen). Same on restart entries, delta entries, and
// uncompressed entries — the variants encode the overflow form
// identically past the key prefix.
const overflowEntryHeaderOverhead = 1 + 2 + 8 + 8

// singleEntryPageOverhead is the leaf-page overhead that an
// otherwise-empty leaf charges for one entry: header (12 bytes —
// page header + DataEnd field) + the smallest valid one-entry
// lookup-table footprint (the compressed-variant restart-table
// entry at 4 bytes; the uncompressed variant's offset-table slot
// is 2 bytes, so picking 4 is the safer over-estimate). Used by
// needsOverflow as the universal "one-entry leaf capacity" budget;
// for the strict per-variant value see internal/page/leaf.go.
const singleEntryPageOverhead = 12 + 4

// needsOverflow reports whether `(key, value)` must be stored as an
// overflow chain. The rule is strict-fit: overflow iff the inline
// encoding cannot fit on an otherwise-empty single-entry leaf page.
// This minimizes overflow chain usage (each chain costs a separate
// page allocation + a 1+N-page IO on Get); profiling can introduce
// a lower threshold later if dense-large-value workloads benefit
// from earlier promotion.
//
// At needsOverflow's threshold (key + value just exceeds the
// single-entry capacity), the resulting leaf has exactly 1 entry
// per page — space-inefficient but functionally correct.
func needsOverflow(cfg page.Config, key, value []byte) bool {
	inlineSize := inlineEntryHeaderOverhead + residentKeyCost(cfg, key) + len(value)
	return inlineSize > cfg.ContentEnd()-singleEntryPageOverhead
}

// residentKeyCost returns the on-leaf byte cost of an entry's key
// half past the fixed header: the full key when it fits the inline
// threshold, or the resident first-T slice plus the 12-byte
// key-extent reference for overflow keys (page-formats.md
// §Overflow-Key Cells).
func residentKeyCost(cfg page.Config, key []byte) int {
	t := cfg.InlineThreshold()
	if len(key) <= t {
		return len(key)
	}
	return t + 12
}

// overflowRefFitsLeaf reports whether an overflow-reference entry
// for `key` fits in a single-entry leaf page. With the overflow-key
// form the resident key cost is capped at InlineThreshold + 12 —
// sized for two-per-branch-page — so this holds at every page size;
// kept as arithmetic (not a constant true) as defense in depth.
func overflowRefFitsLeaf(cfg page.Config, key []byte) bool {
	refSize := overflowEntryHeaderOverhead + residentKeyCost(cfg, key)
	return refSize <= cfg.ContentEnd()-singleEntryPageOverhead
}

// maxKeyTotalLen is the encoding bound on a full key: KeyTotalLen is
// uint32 on the wire (limits.md §Maximum Key Size).
const maxKeyTotalLen = 1<<32 - 1

// keyWithinBound reports whether a key is storable: any length up to
// the uint32 encoding bound — over the inline threshold it takes the
// overflow-key form, never a rejection (limits.md §Maximum Key Size).
// Enforced deterministically at every entry gate: Put, PutEntry, the
// bulk-load builders, and CopyTo's rebuild — one threshold, no drift.
func keyWithinBound(key []byte) bool {
	return uint64(len(key)) <= maxKeyTotalLen
}

// KeyWithinBound exports the storable-key gate for the bulk-load
// construction path (package gmdb), which must gate keys identically
// to Put — one threshold, no drift.
func KeyWithinBound(keyLen int) bool {
	return keyLen >= 0 && uint64(keyLen) <= maxKeyTotalLen
}

// NeedsOverflow exports needsOverflow for the bulk-load construction
// path (package gmdb), which must make the SAME base inline-vs-overflow
// decision as the Put path so a value Put would inline, BulkLoad also
// inlines, and a value Put would promote, BulkLoad also promotes — one
// threshold, no drift. needsOverflow is conservative (over-estimates leaf
// overhead), so anything it reports as inline definitely fits an
// otherwise-empty leaf built by the same page.LeafBuilder.
//
// The Put split path may *additionally* promote an already-inline value
// to overflow when a leaf is too size-skewed for any two-page split (see
// put.go's store loop / largestInlineEntry). That is an on-demand layer
// above this threshold, not a threshold change: BulkLoad never needs it
// (it builds packed leaves bottom-up without splitting), and the leaf
// records the resulting overflow cell, so the two paths' base decision
// stays identical even though Put can end up with more overflow cells.
func NeedsOverflow(cfg page.Config, key, value []byte) bool {
	return needsOverflow(cfg, key, value)
}

// OverflowRefFitsLeaf exports overflowRefFitsLeaf for the bulk-load path.
// False ⇒ the key is too large even for the overflow-reference form
// (the ErrKeyTooLarge surface).
func OverflowRefFitsLeaf(cfg page.Config, key []byte) bool {
	return overflowRefFitsLeaf(cfg, key)
}

// MaxSetCellValueForKey returns the byte budget for a MultiValue
// cell's inline value (its subpage bytes) under `key`: half a
// single-entry leaf's capacity, minus the entry header and the key's
// resident cost. The half-capacity cap is what keeps every leaf
// SPLITTABLE: a two-way split can isolate only an END cell, so a
// mid-leaf cell larger than half a page can make every contiguous
// two-partition overflow — and unlike plain inline values, subpage
// bytes have no overflow form to promote out of the way (the escape
// valve putReportCore's size-skew retry uses). With every cell at or
// under half capacity, the greedy split boundary always leaves both
// halves fitting (left fills past capacity-minus-half before
// declining, so the remainder is at most half plus half). The set
// surface promotes a subpage to a nested tree when it would exceed
// min(page.SubpagePromotionThreshold, this) — the per-key term binds
// for long (overflow-form) keys, whose resident cost consumes most of
// the half-page budget.
func MaxSetCellValueForKey(cfg page.Config, key []byte) int {
	half := (cfg.ContentEnd() - singleEntryPageOverhead) / 2
	return half - inlineEntryHeaderOverhead - residentKeyCost(cfg, key)
}

// writeOverflowChain allocates a contiguous run of pages, encodes
// `value` across them, and returns the new LeafEntry with overflow
// fields set. On error any partial allocation is rolled back via
// FreeRun before the error is surfaced — the chain either lands
// fully or not at all.
func writeOverflowChain(pw PageWriter, cfg page.Config, key, value []byte) (page.LeafEntry, error) {
	runLen := page.OverflowRunLength(cfg, uint64(len(value)))
	firstID, err := pw.AllocContiguous(runLen)
	if err != nil {
		return page.LeafEntry{}, fmt.Errorf("btree: alloc overflow chain (run=%d): %w", runLen, err)
	}
	if err := page.WriteOverflowRun(cfg, value, func(idx uint32, buf []byte) error {
		return pw.WriteRunPage(firstID+uint64(idx), buf)
	}); err != nil {
		_ = pw.FreeRun(firstID, runLen)
		return page.LeafEntry{}, fmt.Errorf("btree: write overflow run: %w", err)
	}
	return page.LeafEntry{
		Flags:        page.CellFlagOverflow,
		Key:          key,
		OverflowPage: firstID,
		TotalLen:     uint64(len(value)),
	}, nil
}

// freeOverflowChainIfPresent retires the overflow chain rooted at
// entry.OverflowPage if entry carries the overflow flag. No-op
// otherwise. Used after a Put-replace or a Delete to release the
// chain owned by the displaced leaf entry; called AFTER the leaf-
// level CoW lands so a transient failure can't orphan the chain.
func freeOverflowChainIfPresent(pw PageWriter, cfg page.Config, entry page.LeafEntry) error {
	if !entry.IsOverflow() {
		return nil
	}
	runLen := page.OverflowRunLength(cfg, entry.TotalLen)
	if err := pw.FreeRun(entry.OverflowPage, runLen); err != nil {
		return fmt.Errorf("btree: free overflow chain at %d (run=%d): %w", entry.OverflowPage, runLen, err)
	}
	return nil
}

// keyTail returns the page.TailCompare that resolves overflow-key
// comparisons over pr (page-formats.md §Overflow-Key Cells,
// Comparison). Precondition per the type's contract: probe's first
// T bytes equal the stored key's resident bytes and len(probe) > T.
// The extent is one contiguous byte range in the run image
// (page-formats.md §Overflow Page), so the comparison is a single
// bytes.Compare over the borrowed slice — no materialization, no
// per-page chunking.
func keyTail(pr PageReader, cfg page.Config) page.TailCompare {
	t := cfg.InlineThreshold()
	return func(probe []byte, extPage uint64, totalLen uint32) (int, error) {
		probeTail := probe[t:]
		storedLen := int(totalLen) - t
		if storedLen <= 0 {
			return 0, fmt.Errorf("%w: overflow-key extent at %d with KeyTotalLen %d <= inline threshold %d",
				ErrCorrupted, extPage, totalLen, t)
		}
		stored, err := readRunExtent(pr, cfg, extPage, uint64(storedLen))
		if err != nil {
			return 0, err
		}
		k := min(len(probeTail), storedLen)
		if c := bytes.Compare(probeTail[:k], stored[:k]); c != 0 {
			return c, nil
		}
		switch {
		case len(probeTail) < storedLen:
			return -1, nil // probe is a strict prefix of the stored key
		case len(probeTail) > storedLen:
			return 1, nil // stored is a strict prefix of the probe
		}
		return 0, nil
	}
}

// readRunExtent fetches the run at headID and returns its first
// extentLen extent bytes as a borrowed slice, after cross-checking
// the head's AdditionalPages against the extentLen-derived run length
// (checksums.md §Structural and Allocation Bounds): the run image is
// bounded by the file-resident extent inside PageRun, so a forged
// extent length that disagrees with the physical run is rejected here
// with no extentLen-sized allocation anywhere on the path.
func readRunExtent(pr PageReader, cfg page.Config, headID uint64, extentLen uint64) ([]byte, error) {
	run, err := pr.PageRun(headID)
	if err != nil {
		return nil, err
	}
	additional, err := page.DecodeOverflowFirstPage(run)
	if err != nil {
		return nil, fmt.Errorf("%w: overflow run at %d: %w", ErrCorrupted, headID, err)
	}
	// Forged-length bound: uint64 run length — a forged extent length
	// whose uint32 run truncates to a small value is caught by the
	// uint64 comparison against the physical (file-bounded) run.
	want := page.OverflowRunLength64(cfg, extentLen)
	if uint64(additional)+1 != want {
		return nil, fmt.Errorf("%w: overflow run at %d: header AdditionalPages %d+1 disagrees with the reference-derived run %d",
			ErrCorrupted, headID, additional, want)
	}
	extent := page.OverflowRunExtent(run, cfg)
	// Guaranteed by the run-length agreement above; kept as defense in
	// depth at the slice boundary.
	if extentLen > uint64(len(extent)) {
		return nil, fmt.Errorf("%w: overflow run at %d: extent length %d exceeds run capacity %d",
			ErrCorrupted, headID, extentLen, len(extent))
	}
	return extent[:extentLen], nil
}

// writeKeyExtent allocates and encodes the key extent holding
// key[T:] and returns the run's first page ID. Mirrors
// writeOverflowChain's all-or-nothing allocation discipline.
func writeKeyExtent(pw PageWriter, cfg page.Config, key []byte) (uint64, error) {
	t := cfg.InlineThreshold()
	tail := key[t:]
	runLen := page.OverflowRunLength(cfg, uint64(len(tail)))
	firstID, err := pw.AllocContiguous(runLen)
	if err != nil {
		return 0, fmt.Errorf("btree: alloc key extent (run=%d): %w", runLen, err)
	}
	if err := page.WriteOverflowRun(cfg, tail, func(idx uint32, buf []byte) error {
		return pw.WriteRunPage(firstID+uint64(idx), buf)
	}); err != nil {
		_ = pw.FreeRun(firstID, runLen)
		return 0, fmt.Errorf("btree: write key extent run: %w", err)
	}
	return firstID, nil
}

// keyExtentRunLen returns the page-run length of a key extent given
// the stored full key length.
func keyExtentRunLen(cfg page.Config, keyTotalLen uint32) uint32 {
	t := cfg.InlineThreshold()
	return page.OverflowRunLength(cfg, uint64(int(keyTotalLen)-t))
}

// freeKeyExtentIfPresent retires the key extent of an overflow-key
// leaf entry (no-op otherwise) — the key-half sibling of
// freeOverflowChainIfPresent, with the same after-CoW call
// discipline. Key extents follow the value-overflow lifecycle
// (page-formats.md §Overflow-Key Cells).
func freeKeyExtentIfPresent(pw PageWriter, cfg page.Config, entry page.LeafEntry) error {
	if !entry.IsOverflowKey() {
		return nil
	}
	runLen := keyExtentRunLen(cfg, entry.KeyTotalLen)
	if err := pw.FreeRun(entry.KeyExtPage, runLen); err != nil {
		return fmt.Errorf("btree: free key extent at %d (run=%d): %w", entry.KeyExtPage, runLen, err)
	}
	return nil
}

// clearKeyExtent returns e with its key-half extent fields zeroed and
// the OverflowKey bit dropped — the shape PutEntry hands back after
// retiring the displaced entry's key extent, so no caller can
// double-free it. The resident Key bytes and every value-half field
// are untouched.
func clearKeyExtent(e page.LeafEntry) page.LeafEntry {
	e.Flags &^= page.CellFlagOverflowKey
	e.KeyExtPage = 0
	e.KeyTotalLen = 0
	return e
}

// FreeKeyExtentIfPresent exports freeKeyExtentIfPresent for the
// keyspace layers' DeleteRange per-cell callbacks (package gmdb),
// which retire per-entry resources outside the btree walkers.
func FreeKeyExtentIfPresent(pw PageWriter, cfg page.Config, entry page.LeafEntry) error {
	return freeKeyExtentIfPresent(pw, cfg, entry)
}

// freeBranchCellExtentIfPresent retires the key extent of an overflow
// branch cell (no-op otherwise). Called when a separator is REMOVED or
// REPLACED — never on a separator move, which carries the extent by
// reference (page-formats.md §Overflow-Key Cells, Branch form).
func freeBranchCellExtentIfPresent(pw PageWriter, cfg page.Config, c page.BranchCell) error {
	if !c.IsOverflowKey() {
		return nil
	}
	runLen := keyExtentRunLen(cfg, c.KeyTotalLen)
	if err := pw.FreeRun(c.KeyExtPage, runLen); err != nil {
		return fmt.Errorf("btree: free branch-separator key extent at %d (run=%d): %w", c.KeyExtPage, runLen, err)
	}
	return nil
}

// readKeyExtentTail returns key[T:] from a key extent as a borrowed
// slice of the run image. Shared by leaf-entry and branch-cell key
// materialization.
func readKeyExtentTail(pr PageReader, cfg page.Config, extPage uint64, keyTotalLen uint32) ([]byte, error) {
	t := cfg.InlineThreshold()
	tailLen := int(keyTotalLen) - t
	if tailLen <= 0 {
		return nil, fmt.Errorf("%w: key extent at %d with KeyTotalLen %d <= inline threshold %d",
			ErrCorrupted, extPage, keyTotalLen, t)
	}
	return readRunExtent(pr, cfg, extPage, uint64(tailLen))
}

// materializeEntryKey returns the FULL key of a leaf entry — the
// resident bytes for ordinary entries; resident + extent tail for
// overflow-key entries. The result is freshly allocated when an
// extent read occurs, otherwise it aliases e.Key.
func materializeEntryKey(pr PageReader, cfg page.Config, e page.LeafEntry) ([]byte, error) {
	if !e.IsOverflowKey() {
		return e.Key, nil
	}
	tail, err := readKeyExtentTail(pr, cfg, e.KeyExtPage, e.KeyTotalLen)
	if err != nil {
		return nil, err
	}
	full := make([]byte, 0, int(e.KeyTotalLen))
	full = append(full, e.Key...)
	full = append(full, tail...)
	return full, nil
}

// MaterializeEntryKey exports materializeEntryKey for the CopyTo
// rebuilders (package gmdb), whose set-tree walk receives raw leaf
// entries and must recover full outer keys before re-accumulating.
func MaterializeEntryKey(pr PageReader, cfg page.Config, e page.LeafEntry) ([]byte, error) {
	return materializeEntryKey(pr, cfg, e)
}

// materializeCellKey is materializeEntryKey for branch cells.
func materializeCellKey(pr PageReader, cfg page.Config, c page.BranchCell) ([]byte, error) {
	if !c.IsOverflowKey() {
		return c.Key, nil
	}
	tail, err := readKeyExtentTail(pr, cfg, c.KeyExtPage, c.KeyTotalLen)
	if err != nil {
		return nil, err
	}
	full := make([]byte, 0, int(c.KeyTotalLen))
	full = append(full, c.Key...)
	full = append(full, tail...)
	return full, nil
}

// makeSeparatorCell builds the branch cell for a freshly-computed FULL
// separator: separators within the inline threshold carry the full key;
// longer ones get a key extent written for sep[T:] and carry the
// resident first-T slice (page-formats.md §Overflow-Key Cells, Branch
// form). The extent is written exactly once here; every later move
// carries it by reference.
func makeSeparatorCell(pw PageWriter, cfg page.Config, sep []byte, child uint64) (page.BranchCell, error) {
	t := cfg.InlineThreshold()
	if len(sep) <= t {
		return page.BranchCell{Key: sep, Child: child}, nil
	}
	ext, err := writeKeyExtent(pw, cfg, sep)
	if err != nil {
		return page.BranchCell{}, err
	}
	return page.BranchCell{
		Key:         sep[:t],
		Child:       child,
		KeyExtPage:  ext,
		KeyTotalLen: uint32(len(sep)),
	}, nil
}

// readOverflowValue returns the value bytes of the overflow run
// rooted at entry.OverflowPage as a single slice of length
// entry.TotalLen. For a committed run this is a BORROWED view of the
// contiguous mmap extent, valid until the transaction closes exactly
// like an inline value; a run written in this same write tx comes
// back as a freshly-allocated assembly of its slab buffers
// (api-surface.md §Byte Slice Ownership). Forged TotalLen / forged
// AdditionalPages are rejected inside readRunExtent with no
// TotalLen-sized allocation on the path (checksums.md §Structural
// and Allocation Bounds).
func readOverflowValue(pr PageReader, cfg page.Config, entry page.LeafEntry) ([]byte, error) {
	if !entry.IsOverflow() {
		return nil, fmt.Errorf("btree: readOverflowValue called on non-overflow entry")
	}
	return readRunExtent(pr, cfg, entry.OverflowPage, entry.TotalLen)
}

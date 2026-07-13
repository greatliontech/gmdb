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
	pages, err := pw.ZeroPageRun(firstID, runLen)
	if err != nil {
		_ = pw.FreeRun(firstID, runLen)
		return page.LeafEntry{}, fmt.Errorf("btree: alloc overflow slab run: %w", err)
	}
	if err := page.EncodeOverflowRun(pages, cfg, value); err != nil {
		_ = pw.FreeRun(firstID, runLen)
		return page.LeafEntry{}, fmt.Errorf("btree: encode overflow run: %w", err)
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
// The comparison streams the extent run one page at a time — never
// materializing the stored tail — with the same forged-length
// discipline as readOverflowValue.
func keyTail(pr PageReader, cfg page.Config) page.TailCompare {
	t := cfg.InlineThreshold()
	firstCap := page.OverflowFirstPageCapacity(cfg)
	followCap := page.OverflowFollowerCapacity(cfg)
	return func(probe []byte, extPage uint64, totalLen uint32) (int, error) {
		probeTail := probe[t:]
		storedTail := int(totalLen) - t
		if storedTail <= 0 {
			return 0, fmt.Errorf("%w: overflow-key extent at %d with KeyTotalLen %d <= inline threshold %d",
				ErrCorrupted, extPage, totalLen, t)
		}
		remaining := storedTail
		pi := uint64(0)
		pos := 0 // consumed bytes of probeTail
		for remaining > 0 {
			buf, err := pr.Page(extPage + pi)
			if err != nil {
				return 0, err
			}
			chunk := followCap
			start := 0
			if pi == 0 {
				chunk = firstCap
				start = page.HeaderSize
			}
			if chunk > remaining {
				chunk = remaining
			}
			stored := buf[start : start+chunk]
			k := min(len(probeTail)-pos, chunk)
			if c := bytes.Compare(probeTail[pos:pos+k], stored[:k]); c != 0 {
				return c, nil
			}
			pos += k
			if k < chunk {
				// probe exhausted while stored bytes remain — probe is
				// a strict prefix of the stored key.
				return -1, nil
			}
			remaining -= chunk
			if pos == len(probeTail) && remaining > 0 {
				// Probe exhausted exactly at a page boundary with
				// stored bytes remaining — strict prefix; skip the
				// pointless zero-byte compare of the next page.
				return -1, nil
			}
			pi++
		}
		// Stored tail exhausted with every byte tied.
		if pos < len(probeTail) {
			return 1, nil // probe longer — stored is a strict prefix
		}
		return 0, nil
	}
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
	pages, err := pw.ZeroPageRun(firstID, runLen)
	if err != nil {
		_ = pw.FreeRun(firstID, runLen)
		return 0, fmt.Errorf("btree: alloc key extent slab run: %w", err)
	}
	if err := page.EncodeOverflowRun(pages, cfg, tail); err != nil {
		_ = pw.FreeRun(firstID, runLen)
		return 0, fmt.Errorf("btree: encode key extent run: %w", err)
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

// readKeyExtentTail assembles key[T:] from a key extent. Shared by
// leaf-entry and branch-cell key materialization.
func readKeyExtentTail(pr PageReader, cfg page.Config, extPage uint64, keyTotalLen uint32) ([]byte, error) {
	t := cfg.InlineThreshold()
	tailLen := int(keyTotalLen) - t
	if tailLen <= 0 {
		return nil, fmt.Errorf("%w: key extent at %d with KeyTotalLen %d <= inline threshold %d",
			ErrCorrupted, extPage, keyTotalLen, t)
	}
	fake := page.LeafEntry{
		Flags:        page.CellFlagOverflow,
		OverflowPage: extPage,
		TotalLen:     uint64(tailLen),
	}
	return readOverflowValue(pr, cfg, fake)
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

// readOverflowValue assembles the value bytes from the overflow
// chain rooted at entry.OverflowPage. Returns a heap-allocated
// slice of length entry.TotalLen — caller-owned, independent of
// the pager's slab / mmap lifetimes. (See api-surface.md §Byte
// Slice Ownership: overflow values diverge from the
// borrowed-reference rule because the value spans non-contiguous
// regions of the mmap (per-page headers / footers); a heap copy
// is the simplest correct shape. Profile-revisit if allocation
// pressure becomes material.)
func readOverflowValue(pr PageReader, cfg page.Config, entry page.LeafEntry) ([]byte, error) {
	if !entry.IsOverflow() {
		return nil, fmt.Errorf("btree: readOverflowValue called on non-overflow entry")
	}
	// Forged-length bound (checksums.md §Structural and Allocation Bounds): a forged on-disk TotalLen can imply a run of billions of
	// pages (OverflowRunLength truncates to uint32, so a naive
	// run-vs-extent guard would pass while make([]byte, TotalLen) is
	// enormous). Compute the run length in uint64 and read pages ONE AT A
	// TIME — pr.Page bounds each id against the file-resident extent
	// (checksums.md §Structural and Allocation Bounds), so a run that walks past the file aborts here, before the
	// TotalLen-sized allocation. Do not pre-size `pages` to run64 (itself
	// possibly forged-huge); a run that stays in-bounds is ≤ the file's
	// page count, which bounds TotalLen ≤ file size for the assembly.
	run64 := page.OverflowRunLength64(cfg, entry.TotalLen)
	pages := make([][]byte, 0, int(min(run64, 64)))
	for i := range run64 {
		buf, err := pr.Page(entry.OverflowPage + i)
		if err != nil {
			return nil, err
		}
		pages = append(pages, buf)
	}
	dst := make([]byte, entry.TotalLen)
	n, err := page.AssembleOverflowValue(pages, cfg, dst)
	if err != nil {
		return nil, fmt.Errorf("%w: overflow chain at %d: %w", ErrCorrupted, entry.OverflowPage, err)
	}
	if uint64(n) != entry.TotalLen {
		return nil, fmt.Errorf("%w: overflow chain at %d short-assembled %d of %d bytes",
			ErrCorrupted, entry.OverflowPage, n, entry.TotalLen)
	}
	return dst, nil
}

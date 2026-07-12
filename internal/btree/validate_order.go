package btree

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/greatliontech/gmdb/internal/page"
)

// ErrValidateStopped is returned by ValidateOrder when the report
// callback asked to stop.
var ErrValidateStopped = errors.New("btree: order validation stopped")

// OrderViolationKind classifies a ValidateOrder finding.
type OrderViolationKind int

const (
	// OrderKeys: leaf or branch keys not strictly increasing, or a
	// key outside its routing range.
	OrderKeys OrderViolationKind = iota
	// OrderNestedCount: a nested-tree cell's NestedCount disagrees
	// with its subtree's actual member count.
	OrderNestedCount
)

// ValidateOrder recursively verifies the ordering invariants of the
// tree rooted at root — the corruption classes a checksum cannot catch
// and the per-page structural Validate deliberately leaves to a
// tree-level pass (page-formats.md separator-routing invariant,
// range-delete.md §Invariants):
//
//   - branch separator keys strictly increase and lie within the
//     parent's key range;
//   - separator routing: every key in the subtree left of separator S
//     is < S, every key right of it is >= S (threaded as half-open
//     [lo, hi) bounds down the recursion);
//   - leaf keys strictly increase and lie within the leaf's bounds;
//   - a nested-tree cell's NestedCount equals the actual member count
//     of its subtree (set-keyspace.md entailed invariant E1's read
//     side), with the nested tree order-validated recursively.
//
// Returns (entries, values): entries counts top-level leaf entries
// (a Keyspace's desc.Count unit); values counts per-cell values —
// 1 for plain/overflow cells, the subpage Count for subpage cells,
// the ACTUAL member count for nested-tree cells (a SetKeyspace's
// desc.Count unit). fvs is the keyspace's FixedValueSize (0 for
// plain keyspaces), needed to read subpage headers.
//
// Violations are reported through report (return false to stop, which
// surfaces ErrValidateStopped); structural failures (unreadable /
// malformed pages) return an error — the reachability walk has its
// own reporting for those, so callers typically ignore duplicates.
// Bounds keys are owned slices (DecodeBranch reconstructs full
// separators into fresh storage) and the leaf predecessor is copied,
// so no borrowed page memory outlives its read.
func ValidateOrder(pr PageReader, cfg page.Config, root, hwm uint64, fvs uint16, report func(kind OrderViolationKind, pageID uint64, msg string) bool) (entries, values uint64, err error) {
	if root == 0 {
		return 0, 0, nil
	}
	err = validateOrderAt(pr, cfg, root, hwm, 0, nil, nil, fvs, report, &entries, &values)
	return entries, values, err
}

func validateOrderAt(pr PageReader, cfg page.Config, pageID, hwm uint64, depth int, lo, hi []byte, fvs uint16, report func(OrderViolationKind, uint64, string) bool, entries, values *uint64) error {
	if depth > MaxTreeDepth {
		return ErrTreeTooDeep
	}
	if pageID >= hwm {
		return fmt.Errorf("%w: page id %d >= HighWaterMark %d at depth %d", ErrCorrupted, pageID, hwm, depth)
	}
	buf, err := pr.Page(pageID)
	if err != nil {
		return err
	}
	inBounds := func(k []byte) bool {
		return (lo == nil || bytes.Compare(k, lo) >= 0) && (hi == nil || bytes.Compare(k, hi) < 0)
	}
	typ, _, _, _ := page.ReadHeader(buf)
	switch {
	case typ == page.TypeBranch:
		if err := page.ValidateBranch(buf, cfg); err != nil {
			return fmt.Errorf("%w: branch %d at depth %d: %w", ErrCorrupted, pageID, depth, err)
		}
		leftmost, cells := page.DecodeBranch(buf, cfg)
		for i, c := range cells {
			if i > 0 && bytes.Compare(cells[i-1].Key, c.Key) >= 0 {
				if !report(OrderKeys, pageID, fmt.Sprintf("branch %d separator[%d] %q not strictly greater than separator[%d] %q",
					pageID, i, c.Key, i-1, cells[i-1].Key)) {
					return ErrValidateStopped
				}
			}
			if !inBounds(c.Key) {
				if !report(OrderKeys, pageID, fmt.Sprintf("branch %d separator[%d] %q outside parent range [%q, %q)",
					pageID, i, c.Key, lo, hi)) {
					return ErrValidateStopped
				}
			}
		}
		// Child i's bounds: [S_{i-1}, S_i) with the parent's lo/hi at
		// the edges — the separator-routing invariant max(left) < S
		// <= min(right) expressed as half-open intervals.
		childLo := lo
		for i := 0; i <= len(cells); i++ {
			childHi := hi
			var child uint64
			if i < len(cells) {
				childHi = cells[i].Key
			}
			if i == 0 {
				child = leftmost
			} else {
				child = cells[i-1].Child
			}
			if child == 0 {
				return fmt.Errorf("%w: null child pointer in branch %d index %d at depth %d", ErrCorrupted, pageID, i, depth)
			}
			if err := validateOrderAt(pr, cfg, child, hwm, depth+1, childLo, childHi, fvs, report, entries, values); err != nil {
				return err
			}
			if i < len(cells) {
				childLo = cells[i].Key
			}
		}
	case page.IsLeafType(typ):
		r := page.NewLeafReader(buf, cfg)
		if err := r.Validate(); err != nil {
			return fmt.Errorf("%w: leaf %d at depth %d: %w", ErrCorrupted, pageID, depth, err)
		}
		it := r.IterForReuse(nil, nil, nil)
		var prev []byte
		havePrev := false
		for {
			e, ok := it.Next()
			if !ok {
				break
			}
			if havePrev && bytes.Compare(prev, e.Key) >= 0 {
				if !report(OrderKeys, pageID, fmt.Sprintf("leaf %d key %q not strictly greater than predecessor %q", pageID, e.Key, prev)) {
					return ErrValidateStopped
				}
			}
			if !inBounds(e.Key) {
				if !report(OrderKeys, pageID, fmt.Sprintf("leaf %d key %q outside routing range [%q, %q)", pageID, e.Key, lo, hi)) {
					return ErrValidateStopped
				}
			}
			// The iterator reuses its key buffer; keep our own copy.
			prev = append(prev[:0], e.Key...)
			havePrev = true
			*entries++
			switch {
			case e.IsNestedTree():
				var nestedEntries, nestedValues uint64
				if err := validateOrderAt(pr, cfg, e.NestedRoot, hwm, depth+1, nil, nil, 0, report, &nestedEntries, &nestedValues); err != nil {
					return err
				}
				if nestedEntries != e.NestedCount {
					if !report(OrderNestedCount, pageID, fmt.Sprintf("leaf %d key %q nested-tree cell claims %d members, subtree holds %d",
						pageID, e.Key, e.NestedCount, nestedEntries)) {
						return ErrValidateStopped
					}
				}
				*values += nestedEntries
			case e.IsSubpage():
				if len(e.Value) < page.SubpageHeaderSize {
					// Reported by the dedicated subpage validation
					// pass; count nothing here.
					continue
				}
				sp := page.NewSubpageReader(e.Value, fvs)
				*values += uint64(sp.Count())
			default:
				*values++
			}
		}
	default:
		return fmt.Errorf("%w: page %d unexpected type %d at depth %d (want branch=%d or leaf=%d/%d)",
			ErrCorrupted, pageID, typ, depth, page.TypeBranch, page.TypeLeaf, page.TypeLeafUncompressed)
	}
	return nil
}

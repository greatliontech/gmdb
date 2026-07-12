// Package compaction holds the incremental-compaction relocation
// core: the consolidating below-floor allocator (Writer) and the
// below-floor reserve formula (Reserve), per
// background-maintenance.md §Incremental Compaction. Pure pager
// mechanics — the pass drivers (forest walk, budget halving,
// commit orchestration) live with the engine.
package compaction

import (
	"errors"

	"github.com/greatliontech/gmdb/internal/btree"
	"github.com/greatliontech/gmdb/internal/pager"
)

// ErrSpaceExhausted aborts a compaction pass when the
// consolidating allocator finds free space ONLY at or above the
// evacuation floor: relocating into the band being drained is the
// no-progress pathology the below-floor policy exists to prevent, so
// the pass rolls back and the driver retries with a halved budget
// (earlier relocations may fit the below-floor capacity) until it
// declines outright.
var ErrSpaceExhausted = errors.New("gmdb: compaction: free space exhausted below the evacuation floor")

// Reserve is the below-floor hole count a compaction pass
// must NOT consume itself: homes for the RPL chain-prefix relocation
// (the full prefix from the deepest at-or-above-floor segment to the
// head) plus the commit's own head-segment append. One formula, used
// by both the floor feasibility scan and the pass's allocation
// Allowance — a divergence between the two would let relocations eat
// the prefix homes the floor was chosen to protect.
func Reserve(pgr *pager.Pager, floor uint64) uint64 {
	return uint64(pgr.RPLRelocationPrefixLen(floor)) + 2
}

// Writer is the relocation pass's PageWriter: allocations
// draw from the LOWEST free hole below AllocBound (the consolidating
// allocator, background-maintenance.md §Incremental Compaction step 2
// — btree.PageWriter's AllocPage contract makes the allocation source
// the writer's concern). Two regimes:
//
//   - strict (the evacuation floor sits above the first data page, so
//     a below-floor region exists): AllocBound = floor.
//   - whole-region (floor at the first data page — the band covers
//     everything, so there is no "below the band" to preserve):
//     AllocBound = HighWaterMark; lowest-hole-first packing still
//     consolidates.
//
// There is NO fallback tier: exhaustion aborts the pass with
// ErrSpaceExhausted. The base allocator's extension tier is
// never a relocation target — extending places LIVE pages at the file
// top, re-creating the band the pass is draining (observed as a
// permanent HWM limit cycle) — and its in-band holes are the refill
// pathology itself. The bound-advance the lazy-shrink clause needs in
// the nothing-reader-safe state comes from the driver's reclaim/
// bound-advance commit (the engine's reclaim/bound-advance step), not from relocating
// into extensions.
type Writer struct {
	// PageWriter is the base adapter (the engine passes its
	// pager-backed btree writer); AllocPage / AllocContiguous are
	// shadowed below with the below-bound consolidating policy.
	btree.PageWriter
	// Pgr serves the below-bound allocation primitives the base
	// interface does not expose.
	Pgr        *pager.Pager
	AllocBound uint64
	// Allowance is the below-bound hole budget for the WHOLE pass —
	// decremented by every allocation (relocated leaves AND their CoW
	// cascades alike; a leaf-count budget alone undercounts and would
	// eat the holes reserved for the RPL prefix relocation's homes).
	Allowance *uint64
}

func (w Writer) AllocPage() (uint64, error) {
	if *w.Allowance == 0 {
		return 0, ErrSpaceExhausted
	}
	id, ok, err := w.Pgr.AllocPageBelow(w.AllocBound)
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, ErrSpaceExhausted
	}
	*w.Allowance--
	return id, nil
}

func (w Writer) AllocContiguous(n uint32) (uint64, error) {
	if *w.Allowance < uint64(n) {
		return 0, ErrSpaceExhausted
	}
	id, ok, err := w.Pgr.AllocContiguousBelow(n, w.AllocBound)
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, ErrSpaceExhausted
	}
	*w.Allowance -= uint64(n)
	return id, nil
}

// compile-time assertion that the consolidating writer satisfies the
// btree page-writer contract (the shadowed Alloc* methods included).
var _ btree.PageWriter = Writer{}

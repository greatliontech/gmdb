package query

import (
	"bytes"
	"container/heap"
	"errors"
	"slices"

	"github.com/thegrumpylion/gmdb/internal/qrep"
)

// ErrQueryMaterializeLimit reports that a buffering node (Sort,
// TopK's heap, hash dedup, the Intersect build side) exceeded the
// query's materialization budget (query-builder.md
// §Materialization budget, Inv-QB6). The iteration fails — never
// silent truncation, sampling, or capping.
var ErrQueryMaterializeLimit = errors.New("gmdb/query: materialization budget exceeded")

// meter tracks one execution's buffered bytes against the query's
// budget. A zero budget never trips (unbounded).
type meter struct {
	limit int
	used  int
}

func (q *Query[K, V]) newMeter() *meter { return &meter{limit: q.budget} }

// charge accounts n retained bytes; false = budget exceeded.
func (m *meter) charge(n int) bool {
	if m.limit <= 0 {
		return true
	}
	m.used += n
	return m.used <= m.limit
}

// refund returns n bytes to the budget (TopK evictions).
func (m *meter) refund(n int) { m.used -= n }

// streamable reports whether the leaf's entry order realizes the
// requested ordering (query-builder.md §Plan nodes: composition
// is driven by the ordering property): the keys must equal the
// index's column sequence from the EQ prefix THROUGH THE LAST
// column — an unmatched tail column would tie-break equal-key
// runs before the PK, breaking Inv-QB5's PK tie-break — with one
// uniform direction (reverse iteration flips whole entries, so
// mixed directions cannot stream; the reversed stream's
// descending-PK equal-key runs are exactly Inv-QB5's directional
// tie-break), and no multi column at or past the prefix
// (needDedup — expansion both duplicates rows and interposes
// element bytes between the keys and the PK).
func streamable(leaf plannedLeaf, order []qrep.OrderKey) (reverse, ok bool) {
	if leaf.shape == shapeScan || leaf.needDedup || len(order) == 0 {
		return false, false
	}
	cols := leaf.index.KeyCols
	prefix := len(leaf.eqVals)
	if len(cols)-prefix != len(order) {
		return false, false
	}
	for i, key := range order {
		if cols[prefix+i].Name != key.ColumnName {
			return false, false
		}
		if key.Desc != order[0].Desc {
			return false, false
		}
	}
	return order[0].Desc, true
}

// sortRow is one buffered row of a materializing ordered node.
type sortRow[K, V any] struct {
	k    K
	v    V
	keys [][]byte
	pk   []byte
	cost int
}

// sortKeysFor computes one row's encoded ordering keys, PK bytes,
// and the retained-byte cost a buffering node charges (Inv-QB6).
func (q *Query[K, V]) sortKeysFor(ops qrep.RowOps, k K, v V) ([][]byte, []byte, int, error) {
	keys := make([][]byte, len(q.order))
	cost := 0
	for i, key := range q.order {
		b, err := key.EncodeRow(k, v)
		if err != nil {
			return nil, nil, 0, err
		}
		keys[i] = b
		cost += len(b)
	}
	pk, err := ops.EncodeKey(k)
	if err != nil {
		return nil, nil, 0, err
	}
	return keys, pk, cost + len(pk), nil
}

// compareRows orders two buffered rows by the encoded key bytes
// (Inv-QB2) with the PK tie-break in the FINAL key's direction
// (Inv-QB5).
func compareRows[K, V any](order []qrep.OrderKey, a, b *sortRow[K, V]) int {
	for i, key := range order {
		c := bytes.Compare(a.keys[i], b.keys[i])
		if key.Desc {
			c = -c
		}
		if c != 0 {
			return c
		}
	}
	c := bytes.Compare(a.pk, b.pk)
	if order[len(order)-1].Desc {
		c = -c
	}
	return c
}

// orderedExec runs an ordered query: the streaming route when the
// (single-leaf) plan's entry order realizes the requested keys,
// else TopK (bounded heap of limit+offset rows) with a Limit, else
// a full Sort — both materializing nodes charge the budget
// (Inv-QB6) and emit with the directional PK tie-break (Inv-QB5).
func (q *Query[K, V]) orderedExec(p queryPlan, hs []branchHandle, yield func(K, V) bool) {
	if p.kind == planLeaf && p.leaf.shape != shapeScan {
		if reverse, ok := streamable(p.leaf, q.order); ok {
			q.drive(p, hs, reverse, q.newMeter(), q.matchPipe(p.residual, q.boundPipe(yield)))
			return
		}
	}
	m := q.newMeter()
	var rows []*sortRow[K, V]
	// TopK: with a limit, only the best limit+offset rows can ever
	// be emitted — the heap holds the CURRENT WORST at its root so
	// a better row evicts it (refunding its budget charge).
	cap := 0
	if q.hasLim {
		cap = q.limit + q.offset
	}
	h := &topKHeap[K, V]{order: q.order}
	ops := q.ks.InternalRowOps()
	collect := func(k K, v V) bool {
		keys, pk, cost, err := q.sortKeysFor(ops, k, v)
		if err != nil {
			q.err = err
			return false
		}
		r := &sortRow[K, V]{k: k, v: v, keys: keys, pk: pk, cost: cost}
		if q.hasLim {
			if len(h.rows) < cap {
				if !m.charge(cost) {
					q.err = ErrQueryMaterializeLimit
					return false
				}
				heap.Push(h, r)
				return true
			}
			// Full: keep r only if it beats the current worst.
			if compareRows(q.order, r, h.rows[0]) < 0 {
				m.refund(h.rows[0].cost)
				if !m.charge(cost) {
					q.err = ErrQueryMaterializeLimit
					return false
				}
				h.rows[0] = r
				heap.Fix(h, 0)
			}
			return true
		}
		if !m.charge(cost) {
			q.err = ErrQueryMaterializeLimit
			return false
		}
		rows = append(rows, r)
		return true
	}
	q.drive(p, hs, false, m, q.matchPipe(p.residual, collect))
	if q.err != nil {
		return
	}
	if q.hasLim {
		rows = h.rows
	}
	slices.SortFunc(rows, func(a, b *sortRow[K, V]) int { return compareRows(q.order, a, b) })
	out := q.boundPipe(yield)
	for _, r := range rows {
		if !out(r.k, r.v) {
			return
		}
	}
}

// topKHeap is a max-heap by the ordered comparator: the root is
// the WORST retained row, evicted when a better one arrives.
type topKHeap[K, V any] struct {
	order []qrep.OrderKey
	rows  []*sortRow[K, V]
}

func (h *topKHeap[K, V]) Len() int { return len(h.rows) }
func (h *topKHeap[K, V]) Less(i, j int) bool {
	return compareRows(h.order, h.rows[i], h.rows[j]) > 0
}
func (h *topKHeap[K, V]) Swap(i, j int) { h.rows[i], h.rows[j] = h.rows[j], h.rows[i] }
func (h *topKHeap[K, V]) Push(x any)    { h.rows = append(h.rows, x.(*sortRow[K, V])) }
func (h *topKHeap[K, V]) Pop() any {
	last := len(h.rows) - 1
	r := h.rows[last]
	h.rows = h.rows[:last]
	return r
}

// Count returns the cardinality of the query's own result —
// exactly len(All()): offset/limit apply via the Inv-QB1 formula
// max(0, min(limit, matched − offset)) — via the cheapest plan
// that can count it, without materializing values where possible
// (query-builder.md §Result semantics). Ordering never changes a
// cardinality and is ignored.
func (q *Query[K, V]) Count() (uint64, error) {
	q.err = nil
	if err := termsErr(q.terms); err != nil {
		q.err = err
		return 0, err
	}
	p, hs := q.resolvePlan(planQuery(q.terms, q.selNames(), q.orderNames(), q.ks.InternalIndexInfo()))
	if q.err != nil {
		return 0, q.err
	}
	var matched uint64
	if p.kind == planLeaf && p.leaf.shape != shapeScan &&
		len(p.residual) == 0 && len(q.filters) == 0 {
		// Value-free counting: entries (with PK dedup where the
		// expansion can repeat a row) without decoding K or V and
		// without any back-lookup.
		idx := hs[0].idx
		var seen map[string]struct{}
		if p.leaf.needDedup {
			seen = make(map[string]struct{})
		}
		m := q.newMeter()
		start, end := entryBounds(p.leaf)
		for ek := range idx.RangeEntries(start, end) {
			if seen != nil {
				s := string(ek.PK)
				if _, dup := seen[s]; dup {
					continue
				}
				// Hash dedup counts against the budget (Inv-QB6).
				if !m.charge(len(s)) {
					q.err = ErrQueryMaterializeLimit
					return 0, q.err
				}
				seen[s] = struct{}{}
			}
			matched++
		}
		if err := idx.Err(); err != nil {
			q.err = err
			return 0, err
		}
	} else {
		q.drive(p, hs, false, q.newMeter(), q.matchPipe(p.residual, func(K, V) bool {
			matched++
			return true
		}))
		if q.err != nil {
			return 0, q.err
		}
	}
	n := int64(matched) - int64(q.offset)
	if n < 0 {
		n = 0
	}
	if q.hasLim {
		lim := int64(q.limit)
		if lim < 0 {
			lim = 0
		}
		if n > lim {
			n = lim
		}
	}
	return uint64(n), nil
}

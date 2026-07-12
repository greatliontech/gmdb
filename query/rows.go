package query

import (
	"errors"
	"fmt"
	"iter"

	"github.com/greatliontech/gmdb"
	"github.com/greatliontech/gmdb/internal/qrep"
	"github.com/greatliontech/gmdb/typed"
)

// errNoSelect marks a Rows() call on a query with no Select — the
// projection surface has nothing to serve.
var errNoSelect = errors.New("gmdb/query: Rows requires a Select")

// Rows yields (K, Projection) for the selected columns
// (query-builder.md §Covering-aware execution). When the chosen
// index carries every selected and residual-term column and no
// opaque filter is present, the plan is INDEX-ONLY: slots decode
// from entry key/covering bytes and the row keyspace is never
// read (route 1). A full-row covering index serves V from its
// entries (route 2); otherwise rows materialize — back-lookup or
// scan (route 3) — and slots compute from the decoded row.
// Results are identical across routes (Inv-QB1; the routes are
// read strategies). Requesting a column the projection does not
// carry errors at Column.From, never a zero value. Terms,
// filters, offset, and limit apply exactly as in All(); check Err
// after iteration.
func (q *Query[K, V]) Rows() iter.Seq2[K, typed.Projection] {
	return func(yield func(K, typed.Projection) bool) {
		q.err = nil
		if len(q.sel) == 0 {
			q.err = errNoSelect
			return
		}
		if err := termsErr(q.terms); err != nil {
			q.err = err
			return
		}
		if q.hasLim && q.limit <= 0 {
			return
		}
		p := planQuery(q.terms, q.selNames(), q.orderNames(), q.ks.InternalIndexInfo())
		if p.kind == planLeaf && p.leaf.shape != shapeScan &&
			entryEligible(p.leaf, q.sel, p.residual, len(q.filters)) {
			// Index-only serves an ordering only when the entry
			// order realizes it (streaming); otherwise the ordered
			// materialized route below is the correct shape.
			if len(q.order) == 0 {
				q.rowsEntryExec(p.leaf, p.residual, false, yield)
				return
			}
			if reverse, ok := streamable(p.leaf, q.order); ok {
				q.rowsEntryExec(p.leaf, p.residual, reverse, yield)
				return
			}
		}
		q.rowsMaterialized(p, yield)
	}
}

// rowsMaterialized is the route-2/3 projection drive: the row
// pipeline (scan, or index with the live-declaration value modes)
// yields surviving (K, V) rows and every slot computes from the
// decoded row via the column's own encoder — the identical bytes
// a covering slot would carry (Inv-TC5's read-side identity).
func (q *Query[K, V]) rowsMaterialized(p queryPlan, yield func(K, typed.Projection) bool) {
	p, hs := q.resolvePlan(p)
	if q.err != nil {
		return
	}
	names := q.selNames()
	project := func(k K, v V) bool {
		slots := make([][]byte, len(q.sel))
		for i, s := range q.sel {
			b, err := s.EncodeRow(k, v)
			if err != nil {
				q.err = fmt.Errorf("gmdb/query: select column encode: %w", err)
				return false
			}
			slots[i] = b
		}
		return yield(k, qrep.ProjectionSlots(names, slots).(typed.Projection))
	}
	if len(q.order) > 0 {
		// Ordered rows emit post-sort, post-bounds; slots compute
		// only for emitted rows.
		q.orderedExec(p, hs, project)
		return
	}
	q.drive(p, hs, false, q.newMeter(), q.matchPipe(p.residual, q.boundPipe(project)))
}

// rowsEntryExec is the index-only route: RangeEntries over the
// leaf's interval, PK dedup where multi-column expansion can
// repeat a row (Inv-QB4), residual evaluation directly over entry
// slot bytes (Inv-QB2 — the same qrep.EvalScalar the row path
// uses), projection slots from entry key/covering bytes. The row
// keyspace is never read. Eligibility was judged on the open-time
// snapshot; the live declaration is re-validated here and any
// shape change falls back to the row-materialized scan, correct
// under every declaration (Inv-QB3).
func (q *Query[K, V]) rowsEntryExec(leaf plannedLeaf, resid []qrep.Term, reverse bool, yield func(K, typed.Projection) bool) {
	idx, err := q.ks.ByteIndex(leaf.index.Name)
	if err != nil {
		q.err = err
		return
	}
	d := idx.Decl()
	keyPos := make(map[string]int, len(d.Columns))
	for i, c := range d.Columns {
		keyPos[c.Name] = i
	}
	coverPos := make(map[string]int, len(d.Covering))
	for i, c := range d.Covering {
		coverPos[c.Name] = i
	}
	resolvable := func(name string) bool {
		if _, ok := keyPos[name]; ok {
			return true
		}
		_, ok := coverPos[name]
		return ok
	}
	liveOK := liveColumnsMatch(leaf.index, d)
	for _, s := range q.sel {
		liveOK = liveOK && resolvable(s.Name)
	}
	var walkResolvable func(ts []qrep.Term) bool
	walkResolvable = func(ts []qrep.Term) bool {
		for _, t := range ts {
			if t.Kind == qrep.KindOr {
				for _, g := range t.Disjuncts {
					if !walkResolvable(g) {
						return false
					}
				}
				continue
			}
			if !resolvable(t.ColumnName) {
				return false
			}
		}
		return true
	}
	liveOK = liveOK && walkResolvable(resid)
	if !liveOK {
		// A same-tx Rebuild changed the tuple or dropped a needed
		// covering column since open: serve row-materialized from
		// a scan — correct under any live shape.
		q.rowsMaterialized(queryPlan{kind: planLeaf, leaf: plannedLeaf{shape: shapeScan}, residual: q.terms}, yield)
		return
	}

	ops := q.ks.InternalRowOps()
	names := q.selNames()
	var seen map[string]struct{}
	if leaf.needDedup {
		seen = make(map[string]struct{})
	}
	m := q.newMeter()
	skipped, yielded := 0, 0
	var opts []gmdb.IterOption
	if reverse {
		opts = append(opts, gmdb.Reverse())
	}
	start, end := entryBounds(leaf)
	for ek, vb := range idx.RangeEntries(start, end, opts...) {
		if seen != nil {
			s := string(ek.PK)
			if _, dup := seen[s]; dup {
				continue
			}
			// Hash dedup counts against the budget (Inv-QB6).
			if !m.charge(len(s)) {
				q.err = ErrQueryMaterializeLimit
				return
			}
			seen[s] = struct{}{}
		}
		// Lazy covering-tuple decode, shared by every slot of this
		// entry.
		var coverCols [][]byte
		slot := func(name string) ([]byte, error) {
			if i, ok := keyPos[name]; ok {
				return ek.Cols[i], nil
			}
			ci, ok := coverPos[name]
			if !ok {
				// Guarded by the resolvability revalidation above;
				// fail loud rather than serve slot 0's bytes.
				return nil, fmt.Errorf("gmdb/query: index %q carries no slot for column %q: %w",
					leaf.index.Name, name, gmdb.ErrCoveringTupleMalformed)
			}
			if coverCols == nil {
				var err error
				if coverCols, err = gmdb.DecodeCoveringTuple(vb); err != nil {
					return nil, err
				}
			}
			if ci >= len(coverCols) {
				return nil, fmt.Errorf("gmdb/query: index %q covering tuple misses slot %d: %w",
					leaf.index.Name, ci, gmdb.ErrCoveringTupleMalformed)
			}
			return coverCols[ci], nil
		}
		match, err := evalTermsSlots(resid, slot)
		if err != nil {
			q.err = err
			return
		}
		if !match {
			continue
		}
		if skipped < q.offset {
			skipped++
			continue
		}
		kAny, err := ops.DecodeKey(ek.PK)
		if err != nil {
			q.err = err
			return
		}
		slots := make([][]byte, len(q.sel))
		for i, s := range q.sel {
			if slots[i], err = slot(s.Name); err != nil {
				q.err = err
				return
			}
		}
		if !yield(kAny.(K), qrep.ProjectionSlots(names, slots).(typed.Projection)) {
			return
		}
		yielded++
		if q.hasLim && yielded >= q.limit {
			return
		}
	}
	if q.err == nil {
		if err := idx.Err(); err != nil {
			q.err = err
		}
	}
}

// evalTermsSlots evaluates a residual conjunction over entry slot
// bytes (index-only route). Every leaf term is scalar and carried
// (entryEligible); Or recurses over its groups.
func evalTermsSlots(terms []qrep.Term, slot func(string) ([]byte, error)) (bool, error) {
	for _, t := range terms {
		ok, err := evalTermSlots(t, slot)
		if err != nil {
			return false, err
		}
		if !ok {
			return false, nil
		}
	}
	return true, nil
}

func evalTermSlots(t qrep.Term, slot func(string) ([]byte, error)) (bool, error) {
	if t.Kind == qrep.KindOr {
		for _, g := range t.Disjuncts {
			all, err := evalTermsSlots(g, slot)
			if err != nil {
				return false, err
			}
			if all {
				return true, nil
			}
		}
		return false, nil
	}
	enc, err := slot(t.ColumnName)
	if err != nil {
		return false, err
	}
	return qrep.EvalScalar(t.Kind, enc, t.Lo, t.Hi), nil
}

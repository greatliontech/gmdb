// Package query is the typed query builder over gmdb/typed
// keyspaces (query-builder.md): structured predicates, index
// selection, and result iteration. This package holds planning
// and execution; the inert declaration-tier value types — Term,
// OrderKey, Projection — live in gmdb/typed with the column
// declarations.
//
// Plans are rule-based (query-builder.md §Planning rules): index
// leaves, Union/Intersect combiners, or the full scan, with
// residual encoded-byte evaluation (Inv-QB2); plan choice is
// never observable in results, only in cost (Inv-QB1).
package query

import (
	"errors"
	"fmt"
	"iter"

	"github.com/greatliontech/gmdb"
	"github.com/greatliontech/gmdb/internal/indexing"
	"github.com/greatliontech/gmdb/internal/qrep"
	"github.com/greatliontech/gmdb/typed"
)

// Query is a builder over one typed keyspace handle. Builder
// methods mutate the query and return the receiver for chaining;
// iteration reflects the builder state at that moment. The query
// is bound to the handle's transaction and inherits its lifetime
// contract.
type Query[K, V any] struct {
	ks      *typed.KeyspaceHandle[K, V]
	terms   []qrep.Term
	filters []func(K, V) bool
	sel     []qrep.SelectCol
	order   []qrep.OrderKey
	budget  int
	limit   int
	hasLim  bool
	offset  int
	err     error
}

// New starts a query over ks.
func New[K, V any](ks *typed.KeyspaceHandle[K, V]) *Query[K, V] {
	return &Query[K, V]{ks: ks}
}

// Where ANDs structured terms onto the query (query-builder.md
// §Terms). A term carrying a literal-encode error fails the query
// at iteration start via Err.
func (q *Query[K, V]) Where(terms ...typed.Term[K, V]) *Query[K, V] {
	for _, t := range terms {
		rep := t.InternalRep()
		if rep.Eval == nil && rep.Kind != qrep.KindOr && rep.Err == nil {
			rep.Err = fmt.Errorf("gmdb/query: zero-value Term (terms are built by column constructors): %w", errBadPredicate)
		}
		q.terms = append(q.terms, rep)
	}
	return q
}

// errBadPredicate marks a structurally unusable predicate — a
// zero-value Term or a nil Filter func.
var errBadPredicate = errors.New("gmdb/query: unusable predicate")

// Filter adds an opaque residual predicate. Filters are never
// pushed down and always see the whole decoded row (Inv-QB7).
func (q *Query[K, V]) Filter(f func(K, V) bool) *Query[K, V] {
	if f == nil {
		q.terms = append(q.terms, qrep.Term{Err: fmt.Errorf("gmdb/query: nil Filter func: %w", errBadPredicate)})
		return q
	}
	q.filters = append(q.filters, f)
	return q
}

// Select names the projection columns Rows() serves
// (query-builder.md §Covering-aware execution). Selected columns
// count toward the planner's covering tie-break, and a plan whose
// index carries every selected and residual-term column serves
// Rows index-only — the row keyspace is never read. Columns are
// single-valued by construction (AnySingleColumn): a multi-valued
// column has no single projection slot and no From surface.
func (q *Query[K, V]) Select(cols ...typed.AnySingleColumn[K, V]) *Query[K, V] {
	for _, c := range cols {
		if c == nil {
			q.terms = append(q.terms, qrep.Term{Err: fmt.Errorf("gmdb/query: nil Select column: %w", errBadPredicate)})
			continue
		}
		q.sel = append(q.sel, c.InternalSelectRep())
	}
	return q
}

// OrderBy sets the result ordering (query-builder.md §Result
// semantics): rows order by the keys' encoded bytes (Inv-QB2),
// ties broken by PK in the FINAL key's direction (Inv-QB5 — the
// ordered sequence is identical across plan choices). An ordering
// the chosen index realizes streams; otherwise the execution
// materializes through TopK (with a Limit) or Sort, counting
// against the materialization budget when one is set (Inv-QB6).
func (q *Query[K, V]) OrderBy(keys ...typed.OrderKey[K, V]) *Query[K, V] {
	for _, k := range keys {
		rep := k.InternalRep()
		if rep.EncodeRow == nil {
			q.terms = append(q.terms, qrep.Term{Err: fmt.Errorf("gmdb/query: zero-value OrderKey (order keys are built by column constructors): %w", errBadPredicate)})
			continue
		}
		q.order = append(q.order, rep)
	}
	return q
}

// WithMaterializeLimit bounds the total bytes buffered by
// buffering nodes for one query execution — Sort, TopK's heap,
// hash dedup, and the Intersect build side (query-builder.md
// §Materialization budget). Accounting basis: the retained
// per-row bytes a node holds (encoded sort keys + PK bytes for
// Sort/TopK; PK bytes for hash sets). Zero (the default) means
// unbounded. Exceeding a set budget fails the iteration with
// ErrQueryMaterializeLimit — never silent truncation (Inv-QB6).
func (q *Query[K, V]) WithMaterializeLimit(bytes int) *Query[K, V] {
	q.budget = bytes
	return q
}

// selNames returns the selected columns' synthesized names.
func (q *Query[K, V]) selNames() []string {
	if len(q.sel) == 0 {
		return nil
	}
	names := make([]string, len(q.sel))
	for i, s := range q.sel {
		names[i] = s.Name
	}
	return names
}

// orderNames returns the order keys' synthesized column names.
func (q *Query[K, V]) orderNames() []string {
	if len(q.order) == 0 {
		return nil
	}
	names := make([]string, len(q.order))
	for i, o := range q.order {
		names[i] = o.ColumnName
	}
	return names
}

// Limit caps the result count. Applied to the final sequence,
// after offset (Inv-QB1). Negative n yields nothing.
func (q *Query[K, V]) Limit(n int) *Query[K, V] {
	q.limit = n
	q.hasLim = true
	return q
}

// Offset skips the first n results of the final sequence.
// Non-positive n skips nothing (normalized to zero here — every
// consumption site reads the stored value arithmetically).
func (q *Query[K, V]) Offset(n int) *Query[K, V] {
	if n < 0 {
		n = 0
	}
	q.offset = n
	return q
}

// Err reports the first error of the last iteration (or a
// construction error), matching the house iterator convention.
func (q *Query[K, V]) Err() error { return q.err }

// termsErr surfaces any carried literal-encode error
// (query-builder.md §Terms: fail at iteration start, before any
// row work). Or terms LIFT their disjuncts' first carried error
// onto their own rep at construction (typed.Or), so a top-level
// scan is complete — no disjunct walk, one source of truth.
func termsErr(terms []qrep.Term) error {
	for _, t := range terms {
		if t.Err != nil {
			return t.Err
		}
	}
	return nil
}

// evalTerm evaluates one term residually (Inv-QB2); Or recurses
// over its conjunction groups.
func evalTerm[K, V any](t qrep.Term, k K, v V) (bool, error) {
	if t.Kind == qrep.KindOr {
		for _, g := range t.Disjuncts {
			all := true
			for _, gt := range g {
				ok, err := evalTerm[K, V](gt, k, v)
				if err != nil {
					return false, err
				}
				if !ok {
					all = false
					break
				}
			}
			if all {
				return true, nil
			}
		}
		return false, nil
	}
	return t.Eval(k, v)
}

// All yields the matching (K, V) rows via the planner's chosen
// access path (query-builder.md §Planning rules); order is
// plan-defined (Inv-QB5 — deterministic per query, not canonical
// across plans; plan choice is never observable in results,
// Inv-QB1). Check Err after iteration: a mid-iteration cursor,
// index, or decode error SURFACES via Err — a truncated result is
// never silently indistinguishable from a small one (Inv-QB1's
// forbidden class). A limit-complete result ends iteration at the
// cap: rows past it are outside the observable sequence and never
// read, so they cannot contribute errors.
func (q *Query[K, V]) All() iter.Seq2[K, V] {
	return func(yield func(K, V) bool) {
		q.err = nil
		if err := termsErr(q.terms); err != nil {
			q.err = err
			return
		}
		// The limit caps the observable sequence (Inv-QB1's
		// cardinality formula): rows past it are unreachable, so a
		// non-positive limit never opens a cursor or handle and the
		// executors return at the limit-th yield — work and ERRORS
		// beyond the cap must not leak into a complete result.
		if q.hasLim && q.limit <= 0 {
			return
		}
		p, hs := q.resolvePlan(planQuery(q.terms, q.selNames(), q.orderNames(), q.ks.InternalIndexInfo()))
		if q.err != nil {
			return
		}
		if len(q.order) > 0 {
			q.orderedExec(p, hs, yield)
			return
		}
		q.drive(p, hs, false, q.newMeter(), q.matchPipe(p.residual, q.boundPipe(yield)))
	}
}

// resolvePlan obtains fresh byte handles for every index leaf of
// p and validates each live tuple (Inv-QB3): any mismatch after a
// same-tx Rebuild degrades the whole plan to the scan, whose
// residual is the full conjunction — correct under any live shape
// (Inv-QB1). A handle-acquisition error lands on q.err.
func (q *Query[K, V]) resolvePlan(p queryPlan) (queryPlan, []branchHandle) {
	var leaves []plannedLeaf
	switch p.kind {
	case planUnion:
		for _, b := range p.branches {
			leaves = append(leaves, b.leaf)
		}
	case planIntersect:
		leaves = []plannedLeaf{p.probe, p.build}
	default:
		if p.leaf.shape == shapeScan {
			return p, nil
		}
		leaves = []plannedLeaf{p.leaf}
	}
	hs, ok := q.openBranches(leaves)
	if !ok || q.err != nil {
		return queryPlan{kind: planLeaf, leaf: plannedLeaf{shape: shapeScan}, residual: q.terms}, nil
	}
	return p, hs
}

// drive pushes every row of the RESOLVED plan through tail. m is
// the execution's ONE budget meter (query-builder.md
// §Materialization budget bounds the TOTAL buffered bytes per
// execution — every buffering node shares it).
func (q *Query[K, V]) drive(p queryPlan, hs []branchHandle, reverse bool, m *meter, tail func(K, V) bool) {
	switch p.kind {
	case planUnion:
		q.unionDrive(p, hs, m, tail)
	case planIntersect:
		q.intersectDrive(p, hs, m, tail)
	default:
		if p.leaf.shape == shapeScan {
			q.scanDrive(tail)
			return
		}
		q.leafDrive(hs[0].idx, hs[0].d, p.leaf, nil, reverse, m, func(_ string, k K, v V) bool { return tail(k, v) })
	}
}

// Explain returns the chosen plan as a value without executing
// (query-builder.md §Query surface) — the plan-pinning surface.
// Explain plans regardless of carried construction errors (a term
// with a literal-encode error still has a plan shape); every
// EXECUTION of such a query fails at iteration start via Err.
// Leaf value routes are derived from the handle's open-time
// declarations; execution re-derives against the live declaration
// (Inv-QB3) — results are identical, routes are read strategies.
func (q *Query[K, V]) Explain() Plan {
	p := planQuery(q.terms, q.selNames(), q.orderNames(), q.ks.InternalIndexInfo())
	var root PlanNode
	switch p.kind {
	case planUnion:
		branches := make([]PlanNode, len(p.branches))
		for i, b := range p.branches {
			var bn PlanNode = leafNode(b.leaf, snapshotRoute(b.leaf))
			if len(b.resid) > 0 {
				bn = ResidualFilter{Input: bn, Terms: len(b.resid)}
			}
			branches[i] = bn
		}
		root = Union{Branches: branches, Merge: p.merge}
	case planIntersect:
		root = Intersect{
			Probe: leafNode(p.probe, snapshotRoute(p.probe)),
			Build: leafNode(p.build, snapshotRoute(p.build)),
		}
	default:
		route := ValuesRowBytes
		if p.leaf.shape != shapeScan {
			if entryEligible(p.leaf, q.sel, p.residual, len(q.filters)) {
				route = ValuesEntry
			} else {
				route = snapshotRoute(p.leaf)
			}
		}
		root = leafNode(p.leaf, route)
	}
	if len(p.residual) > 0 || len(q.filters) > 0 {
		root = ResidualFilter{Input: root, Terms: len(p.residual), Filters: len(q.filters)}
	}
	if len(q.order) > 0 {
		streamed := false
		if p.kind == planLeaf && p.leaf.shape != shapeScan {
			if reverse, ok := streamable(p.leaf, q.order); ok {
				streamed = true
				if reverse {
					// Mark the streaming-descending drain on the leaf.
					root = markReverse(root)
				}
			}
		}
		if !streamed {
			if q.hasLim {
				root = TopK{Input: root, K: q.limit + q.offset}
			} else {
				root = Sort{Input: root}
			}
		}
	}
	if len(q.sel) > 0 {
		root = Project{Input: root, Columns: len(q.sel)}
	}
	return Plan{Root: root}
}

// markReverse flips the Reverse marker on the plan's index leaf
// (possibly under a ResidualFilter).
func markReverse(n PlanNode) PlanNode {
	switch t := n.(type) {
	case ResidualFilter:
		t.Input = markReverse(t.Input)
		return t
	case IndexPrefix:
		t.Reverse = true
		return t
	case IndexRange:
		t.Reverse = true
		return t
	}
	return n
}

// leafNode renders one planned leaf as its public node.
func leafNode(leaf plannedLeaf, route ValueRoute) PlanNode {
	switch leaf.shape {
	case shapeSeek:
		return IndexSeek{Index: leaf.index.Name, Values: route}
	case shapePrefix:
		return IndexPrefix{Index: leaf.index.Name, PrefixLen: len(leaf.eqVals), Values: route}
	case shapeRange:
		return IndexRange{Index: leaf.index.Name, PrefixLen: len(leaf.eqVals), Values: route}
	}
	return Scan{}
}

// snapshotRoute derives a leaf's row-serving value route from the
// open-time declaration snapshot (Explain's view; execution
// re-derives live — Inv-QB3).
func snapshotRoute(leaf plannedLeaf) ValueRoute {
	switch {
	case leaf.index.CoverValue:
		return ValuesCoverValue
	case len(leaf.index.Covering) > 0:
		return ValuesBackLookup
	}
	return ValuesRowBytes
}

// scanDrive iterates the whole keyspace via the typed cursor,
// pushing EVERY row through tail (residuals, filters, and bounds
// live in the composed pipeline). The scan is ascending-only
// (query-builder.md §Plan nodes) — descending orders materialize.
func (q *Query[K, V]) scanDrive(tail func(K, V) bool) {
	c := q.ks.Cursor()
	// Each execution opens a fresh cursor; Close releases its
	// staleness registration so repeated executions in one long
	// transaction don't accumulate per-mutation tracking cost.
	defer c.Close()
	for k, v, ok := c.First(); ok; k, v, ok = c.Next() {
		if !tail(k, v) {
			return
		}
	}
	if err := c.Err(); err != nil {
		q.err = err
	}
}

// valueMode is leafDrive's per-entry value-acquisition strategy,
// derived from the LIVE declaration.
type valueMode int

const (
	modeRowBytes valueMode = iota // entry value = row bytes
	modeFetch                     // back-lookup via the row keyspace
	modeCover                     // full-row covering: V from the entry tuple
)

// matchPipe is the match stage of the execution tail: outer
// residual terms (Inv-QB2) then opaque filters (Inv-QB7 — they
// see whole rows). Matching rows continue to next; the returned
// func reports false to STOP the drain.
func (q *Query[K, V]) matchPipe(resid []qrep.Term, next func(K, V) bool) func(K, V) bool {
	return func(k K, v V) bool {
		for _, t := range resid {
			ok, err := evalTerm[K, V](t, k, v)
			if err != nil {
				q.err = err
				return false
			}
			if !ok {
				return true
			}
		}
		for _, f := range q.filters {
			if !f(k, v) {
				return true
			}
		}
		return next(k, v)
	}
}

// boundPipe is the bound stage: offset then limit, applied to the
// FINAL sequence (Inv-QB1) — after any ordering.
func (q *Query[K, V]) boundPipe(yield func(K, V) bool) func(K, V) bool {
	skipped, yielded := 0, 0
	return func(k K, v V) bool {
		if skipped < q.offset {
			skipped++
			return true
		}
		if !yield(k, v) {
			return false
		}
		yielded++
		return !(q.hasLim && yielded >= q.limit)
	}
}

// leafDrive is the index-leaf row core shared by single-leaf,
// Union-branch, and Intersect executions: a FRESH byte handle per
// leaf per execution (per-handle Err state makes sharing between
// concurrently-draining iterators mutually clobbering —
// query-builder.md §Plan nodes), PK dedup where multi-column
// expansion can repeat a row (Inv-QB4), residual evaluation of
// the supplied terms (Inv-QB2), and value acquisition per the
// LIVE declaration (Inv-QB3): entry value bytes ARE the row bytes
// for a non-covering index; a full-row covering entry whose live
// sentinel embeds THIS handle's value-encoder ID serves V from
// the entry; any other covering declaration back-looks-up
// (mirroring byte Lookup's silent-skip of vanished rows). The
// caller has already validated liveColumnsMatch. yield receives
// (pk key, K, V) for rows passing resid; false stops the drain.
func (q *Query[K, V]) leafDrive(idx *gmdb.IndexHandle, d *gmdb.IndexDecl, leaf plannedLeaf, resid []qrep.Term, reverse bool, m *meter, yield func(string, K, V) bool) {
	var opts []gmdb.IterOption
	if reverse {
		opts = append(opts, gmdb.Reverse())
	}
	ops := q.ks.InternalRowOps()
	mode := modeRowBytes
	switch {
	// The full-row route additionally verifies the live sentinel
	// embeds THIS handle's value-encoder ID: a same-tx Rebuild can
	// install another encoder's cover-value sentinel, whose bytes
	// this codec would decode silently wrong — those entries
	// back-look-up instead (correct under any encoder).
	case indexing.IsCoverValueDecl(d) &&
		d.Covering[0].Name == indexing.CoverValueColumn(ops.ValEncID):
		mode = modeCover
	case len(d.Covering) > 0:
		mode = modeFetch
	}
	var seen map[string]struct{}
	if leaf.needDedup {
		seen = make(map[string]struct{})
	}
	// row processes one index entry; false stops the drain.
	row := func(pk, vb []byte) bool {
		s := string(pk)
		if seen != nil {
			if _, dup := seen[s]; dup {
				return true
			}
			// Leaf-level multi-expansion dedup is hash dedup: its
			// set counts against the budget (Inv-QB6).
			if !m.charge(len(s)) {
				q.err = ErrQueryMaterializeLimit
				return false
			}
			seen[s] = struct{}{}
		}
		kAny, err := ops.DecodeKey(pk)
		if err != nil {
			q.err = err
			return false
		}
		var vAny any
		switch mode {
		case modeRowBytes:
			if vAny, err = ops.DecodeVal(vb); err != nil {
				q.err = err
				return false
			}
		case modeCover:
			// Full-row covering: the entry value is a one-column
			// covering tuple holding encode(V) — no row read
			// (route 2, query-builder.md §Covering-aware execution).
			cols, err := gmdb.DecodeCoveringTuple(vb)
			if err != nil {
				q.err = err
				return false
			}
			if len(cols) == 0 {
				q.err = fmt.Errorf("gmdb/query: index %q: full-row covering tuple has no slots: %w",
					leaf.index.Name, gmdb.ErrCoveringTupleMalformed)
				return false
			}
			if vAny, err = ops.DecodeVal(cols[0]); err != nil {
				q.err = err
				return false
			}
		case modeFetch:
			var found bool
			if vAny, found, err = ops.FetchRow(pk); err != nil {
				q.err = err
				return false
			} else if !found {
				return true
			}
		}
		k, v := kAny.(K), vAny.(V)
		for _, t := range resid {
			ok, err := evalTerm[K, V](t, k, v)
			if err != nil {
				q.err = err
				return false
			}
			if !ok {
				return true
			}
		}
		return yield(s, k, v)
	}
	switch leaf.shape {
	case shapeSeek:
		if mode == modeFetch {
			for pk := range idx.LookupKeys(leaf.eqVals, opts...) {
				if !row(pk, nil) {
					break
				}
			}
		} else {
			for pk, vb := range idx.Lookup(leaf.eqVals, opts...) {
				if !row(pk, vb) {
					break
				}
			}
		}
	case shapePrefix:
		for pk, vb := range idx.Prefix(leaf.eqVals, opts...) {
			if !row(pk, vb) {
				break
			}
		}
	case shapeRange:
		start, end := rangeBounds(leaf.eqVals, leaf.bound)
		for pk, vb := range idx.Range(start, end, opts...) {
			if !row(pk, vb) {
				break
			}
		}
	}
	if q.err == nil {
		if err := idx.Err(); err != nil {
			q.err = err
		}
	}
}

// Keys yields the matching primary keys only.
func (q *Query[K, V]) Keys() iter.Seq[K] {
	return func(yield func(K) bool) {
		for k := range q.All() {
			if !yield(k) {
				return
			}
		}
	}
}

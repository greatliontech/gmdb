// Package query is the typed query builder over gmdb/typed
// keyspaces (query-builder.md): structured predicates, index
// selection, and result iteration. This package holds planning
// and execution; the inert declaration-tier value types — Term,
// OrderKey, Projection — live in gmdb/typed with the column
// declarations.
//
// The current plan shape is the full scan with residual
// encoded-byte evaluation (Inv-QB2); richer plan shapes slot in
// behind the same surface without changing results (Inv-QB1 —
// plan choice is never observable in results, only in cost).
package query

import (
	"errors"
	"fmt"
	"iter"

	"github.com/thegrumpylion/gmdb"
	"github.com/thegrumpylion/gmdb/internal/indexing"
	"github.com/thegrumpylion/gmdb/internal/qrep"
	"github.com/thegrumpylion/gmdb/typed"
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

// Limit caps the result count. Applied to the final sequence,
// after offset (Inv-QB1). Negative n yields nothing.
func (q *Query[K, V]) Limit(n int) *Query[K, V] {
	q.limit = n
	q.hasLim = true
	return q
}

// Offset skips the first n results of the final sequence.
// Non-positive n skips nothing.
func (q *Query[K, V]) Offset(n int) *Query[K, V] {
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
		leaf := planQuery(q.terms, q.selNames(), q.ks.InternalIndexInfo())
		if leaf.shape == shapeScan {
			q.scanExec(yield)
			return
		}
		q.indexExec(leaf, yield)
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
	leaf := planQuery(q.terms, q.selNames(), q.ks.InternalIndexInfo())
	route := ValuesRowBytes
	if leaf.shape != shapeScan {
		resid := residualTerms(q.terms, leaf.consumed)
		switch {
		case entryEligible(leaf, q.sel, resid, len(q.filters)):
			route = ValuesEntry
		case leaf.index.CoverValue:
			route = ValuesCoverValue
		case len(leaf.index.Covering) > 0:
			route = ValuesBackLookup
		}
	}
	var root PlanNode
	switch leaf.shape {
	case shapeSeek:
		root = IndexSeek{Index: leaf.index.Name, Values: route}
	case shapePrefix:
		root = IndexPrefix{Index: leaf.index.Name, PrefixLen: len(leaf.eqVals), Values: route}
	case shapeRange:
		root = IndexRange{Index: leaf.index.Name, PrefixLen: len(leaf.eqVals), Values: route}
	default:
		root = Scan{}
	}
	if resid := len(q.terms) - len(leaf.consumed); resid > 0 || len(q.filters) > 0 {
		root = ResidualFilter{Input: root, Terms: resid, Filters: len(q.filters)}
	}
	if len(q.sel) > 0 {
		root = Project{Input: root, Columns: len(q.sel)}
	}
	return Plan{Root: root}
}

// scanExec is the Scan leaf: full typed-cursor iteration with
// every term and filter residual.
func (q *Query[K, V]) scanExec(yield func(K, V) bool) {
	c := q.ks.Cursor()
	// Each execution opens a fresh cursor; Close releases its
	// staleness registration so repeated executions in one long
	// transaction don't accumulate per-mutation tracking cost.
	defer c.Close()
	skipped, yielded := 0, 0
	for k, v, ok := c.First(); ok; k, v, ok = c.Next() {
		match := true
		for _, t := range q.terms {
			ok, err := evalTerm[K, V](t, k, v)
			if err != nil {
				q.err = err
				return
			}
			if !ok {
				match = false
				break
			}
		}
		if match {
			for _, f := range q.filters {
				if !f(k, v) {
					match = false
					break
				}
			}
		}
		if !match {
			continue
		}
		if skipped < q.offset {
			skipped++
			continue
		}
		if !yield(k, v) {
			return
		}
		yielded++
		if q.hasLim && yielded >= q.limit {
			return
		}
	}
	if err := c.Err(); err != nil {
		q.err = err
	}
}

// indexExec drains one index leaf: a FRESH byte handle per
// execution (per-handle Err state makes sharing between
// concurrently-draining iterators mutually clobbering —
// query-builder.md §Plan nodes), PK dedup when the leaf can yield
// one entry per multi-column element (Inv-QB4), residual
// evaluation of the unconsumed terms (Inv-QB2), and value
// acquisition per the declaration: entry value bytes ARE the row
// bytes for a non-covering index; a covering declaration's entry
// bytes are a covering tuple, so rows back-look-up via the typed
// handle instead (index-only serving is the covering execution
// surface, not this leaf; the back-lookup mirrors byte Lookup's
// silent-skip of vanished rows).
// valueMode is indexExec's per-entry value-acquisition strategy,
// derived from the LIVE declaration.
type valueMode int

const (
	modeRowBytes valueMode = iota // entry value = row bytes
	modeFetch                     // back-lookup via the row keyspace
	modeCover                     // full-row covering: V from the entry tuple
)

func (q *Query[K, V]) indexExec(leaf plannedLeaf, yield func(K, V) bool) {
	idx, err := q.ks.ByteIndex(leaf.index.Name)
	if err != nil {
		q.err = err
		return
	}
	// Value interpretation AND tuple shape follow the LIVE
	// declaration (Inv-QB3), probed on the fresh handle — not the
	// handle's open-time planner snapshot: a same-tx Rebuild can
	// change the covering shape (entry value bytes become a
	// covering TUPLE, never row bytes) or the column tuple itself
	// (the plan's literals would seek wrong entries). A changed
	// tuple falls back to the scan, correct under any shape; the
	// snapshot still drives plan CHOICE — cost-only under Inv-QB1.
	d := idx.Decl()
	if !liveColumnsMatch(leaf.index, d) {
		q.scanExec(yield)
		return
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
	resid := residualTerms(q.terms, leaf.consumed)
	var seen map[string]struct{}
	if leaf.needDedup {
		seen = make(map[string]struct{})
	}
	skipped, yielded := 0, 0
	// row processes one index entry; false stops the drain (error,
	// consumer break, or limit-complete — q.err distinguishes).
	row := func(pk, vb []byte) bool {
		if seen != nil {
			s := string(pk)
			if _, dup := seen[s]; dup {
				return true
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
		for _, f := range q.filters {
			if !f(k, v) {
				return true
			}
		}
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
	switch leaf.shape {
	case shapeSeek:
		if mode == modeFetch {
			for pk := range idx.LookupKeys(leaf.eqVals) {
				if !row(pk, nil) {
					break
				}
			}
		} else {
			for pk, vb := range idx.Lookup(leaf.eqVals) {
				if !row(pk, vb) {
					break
				}
			}
		}
	case shapePrefix:
		for pk, vb := range idx.Prefix(leaf.eqVals) {
			if !row(pk, vb) {
				break
			}
		}
	case shapeRange:
		start, end := rangeBounds(leaf.eqVals, leaf.bound)
		for pk, vb := range idx.Range(start, end) {
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

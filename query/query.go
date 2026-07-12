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

// All yields the matching (K, V) rows. The current plan shape is
// a full scan with residual evaluation; order is plan-defined
// (Inv-QB5 — deterministic per query, not canonical across
// plans). Check Err after iteration: the scan runs on the typed
// cursor so a mid-scan cursor or decode error SURFACES via Err —
// a truncated result is never silently indistinguishable from a
// small one (Inv-QB1's forbidden class). A limit-complete result
// ends the scan at the cap: rows past it are outside the
// observable sequence and never read, so they cannot contribute
// errors.
func (q *Query[K, V]) All() iter.Seq2[K, V] {
	return func(yield func(K, V) bool) {
		q.err = nil
		if err := termsErr(q.terms); err != nil {
			q.err = err
			return
		}
		// The limit caps the observable sequence (Inv-QB1's
		// cardinality formula): rows past it are unreachable, so a
		// non-positive limit never opens a cursor and the loop below
		// returns at the limit-th yield — scan work and scan ERRORS
		// beyond the cap must not leak into a complete result.
		if q.hasLim && q.limit <= 0 {
			return
		}
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

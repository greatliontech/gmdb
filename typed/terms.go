package typed

import (
	"fmt"

	"github.com/greatliontech/gmdb/internal/qrep"
)

// Term is one structured predicate over rows of a Keyspace[K, V]:
// a (column, op, encoded literal) node, or an Or of conjunction
// groups (query-builder.md §Terms). Literals are encoded at term
// construction via the column's own encoder; an encode error is
// carried on the term and fails the query at iteration start.
// Terms are inert declaration-tier values — the planning and
// execution surfaces live in gmdb/query.
type Term[K, V any] struct {
	rep qrep.Term
}

// InternalRep exposes the term's internal representation to the
// query package through the shared internal seam. The returned
// type lives in an internal package: callers outside this module
// cannot name or construct it, so the representation carries no
// compatibility promise (query-builder.md §Terms). The literal
// byte slices are shared, not copied — treat as read-only.
func (t Term[K, V]) InternalRep() qrep.Term { return t.rep }

// OrderKey is one ordering key — a column plus direction
// (query-builder.md §Query surface).
type OrderKey[K, V any] struct {
	rep qrep.OrderKey
}

// InternalRep — see Term.InternalRep.
func (o OrderKey[K, V]) InternalRep() qrep.OrderKey { return o.rep }

// encodeLiteral encodes one literal with the column's encoder,
// folding a failure into the term's carried error.
func encodeLiteral[C any](enc Encoder[C], v C) ([]byte, error) {
	return enc.AppendEncode(nil, v)
}

func (c *Column[K, V, C]) term(kind qrep.Kind, lo, hi []byte, encErr error) Term[K, V] {
	col := c // capture
	return Term[K, V]{rep: qrep.Term{
		Kind:       kind,
		ColumnName: c.columnName(),
		Lo:         lo,
		Hi:         hi,
		Err:        encErr,
		Eval: func(k, v any) (bool, error) {
			enc, err := col.enc.AppendEncode(nil, col.get(k.(K), v.(V)))
			if err != nil {
				return false, fmt.Errorf("gmdb: column %q: residual encode: %w", col.name, err)
			}
			return evalScalar(kind, enc, lo, hi), nil
		},
	}}
}

// evalScalar delegates to the seam's single byte-comparison source
// (qrep.EvalScalar — Inv-QB2: the SAME semantics an index seek and
// the executor's entry-slot evaluation realize, so plan and scan
// agree by construction).
func evalScalar(kind qrep.Kind, enc, lo, hi []byte) bool {
	if !qrep.HandledScalarKind(kind) {
		// Unreachable: Contains kinds are remapped before this call
		// and Or never reaches Eval. A new kind missed here must
		// fail loud, not silently match nothing.
		panic(fmt.Sprintf("gmdb/typed: evalScalar: unhandled term kind %d", kind))
	}
	return qrep.EvalScalar(kind, enc, lo, hi)
}

// Eq matches rows whose column equals v.
func (c *Column[K, V, C]) Eq(v C) Term[K, V] {
	lo, err := encodeLiteral(c.enc, v)
	return c.term(qrep.KindEq, lo, nil, err)
}

// Lt matches rows whose column is below v (byte order — Inv-QB2).
func (c *Column[K, V, C]) Lt(v C) Term[K, V] {
	lo, err := encodeLiteral(c.enc, v)
	return c.term(qrep.KindLt, lo, nil, err)
}

// Lte matches rows whose column is at or below v.
func (c *Column[K, V, C]) Lte(v C) Term[K, V] {
	lo, err := encodeLiteral(c.enc, v)
	return c.term(qrep.KindLte, lo, nil, err)
}

// Gt matches rows whose column is above v.
func (c *Column[K, V, C]) Gt(v C) Term[K, V] {
	lo, err := encodeLiteral(c.enc, v)
	return c.term(qrep.KindGt, lo, nil, err)
}

// Gte matches rows whose column is at or above v.
func (c *Column[K, V, C]) Gte(v C) Term[K, V] {
	lo, err := encodeLiteral(c.enc, v)
	return c.term(qrep.KindGte, lo, nil, err)
}

// Between matches rows whose column is in [lo, hi) — half-open,
// matching the byte Range convention.
func (c *Column[K, V, C]) Between(lo, hi C) Term[K, V] {
	lb, err1 := encodeLiteral(c.enc, lo)
	hb, err2 := encodeLiteral(c.enc, hi)
	if err1 == nil {
		err1 = err2
	}
	return c.term(qrep.KindBetween, lb, hb, err1)
}

// HasPrefix matches rows whose ENCODED column value has the
// encoded literal as a byte prefix — defined purely at the byte
// level, so pushdown and residual evaluation agree (Inv-QB2) for
// every encoder. Byte-prefix coincides with the natural "starts
// with" semantic only for identity-like encoders (the canonical
// StringEncoder and BytesEncoder).
func (c *Column[K, V, C]) HasPrefix(v C) Term[K, V] {
	lo, err := encodeLiteral(c.enc, v)
	return c.term(qrep.KindHasPrefix, lo, nil, err)
}

// Asc orders by this column ascending.
func (c *Column[K, V, C]) Asc() OrderKey[K, V] { return c.orderKey(false) }

// Desc orders by this column descending.
func (c *Column[K, V, C]) Desc() OrderKey[K, V] { return c.orderKey(true) }

func (c *Column[K, V, C]) orderKey(desc bool) OrderKey[K, V] {
	col := c
	return OrderKey[K, V]{rep: qrep.OrderKey{
		ColumnName: c.columnName(),
		Desc:       desc,
		EncodeRow: func(k, v any) ([]byte, error) {
			return col.enc.AppendEncode(nil, col.get(k.(K), v.(V)))
		},
	}}
}

func (m *MultiColumn[K, V, C]) term(kind qrep.Kind, lo, hi []byte, encErr error) Term[K, V] {
	col := m
	return Term[K, V]{rep: qrep.Term{
		Kind:       kind,
		ColumnName: m.columnName(),
		Multi:      true,
		Lo:         lo,
		Hi:         hi,
		Err:        encErr,
		Eval: func(k, v any) (bool, error) {
			for _, e := range col.get(k.(K), v.(V)) {
				enc, err := col.enc.AppendEncode(nil, e)
				if err != nil {
					return false, fmt.Errorf("gmdb: column %q: residual encode: %w", col.name, err)
				}
				scalar := qrep.KindEq
				if kind == qrep.KindContainsRange {
					scalar = qrep.KindBetween
				}
				if evalScalar(scalar, enc, lo, hi) {
					return true, nil
				}
			}
			return false, nil
		},
	}}
}

// Contains matches rows where ANY element of the multi-column
// equals v.
func (m *MultiColumn[K, V, C]) Contains(v C) Term[K, V] {
	lo, err := encodeLiteral(m.enc, v)
	return m.term(qrep.KindContains, lo, nil, err)
}

// ContainsRange matches rows where ANY element of the
// multi-column falls in [lo, hi).
func (m *MultiColumn[K, V, C]) ContainsRange(lo, hi C) Term[K, V] {
	lb, err1 := encodeLiteral(m.enc, lo)
	hb, err2 := encodeLiteral(m.enc, hi)
	if err1 == nil {
		err1 = err2
	}
	return m.term(qrep.KindContainsRange, lb, hb, err1)
}

// The query package constructs Projection values through the
// shared internal seam (typed-columns.md §Covering projections).
func init() {
	qrep.ProjectionSlots = func(names []string, vals [][]byte) any {
		return newProjection(names, vals)
	}
}

// Or builds a disjunction: each group is a conjunction of terms;
// the Or matches rows matching ANY group (query-builder.md
// §Terms).
func Or[K, V any](groups ...[]Term[K, V]) Term[K, V] {
	rep := qrep.Term{Kind: qrep.KindOr}
	for _, g := range groups {
		reps := make([]qrep.Term, len(g))
		for i, t := range g {
			reps[i] = t.rep
			if t.rep.Err != nil && rep.Err == nil {
				rep.Err = t.rep.Err
			}
		}
		rep.Disjuncts = append(rep.Disjuncts, reps)
	}
	return Term[K, V]{rep: rep}
}

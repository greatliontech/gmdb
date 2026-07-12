package query

import (
	"slices"
	"strings"

	"github.com/thegrumpylion/gmdb/internal/qrep"
)

// The rule-based planner (query-builder.md §Planning rules):
// deterministic, not cost-based. This stage plans a single index
// leaf per query — disjunction pushdown (Union) and Intersect are
// combiner rules that compose on top without changing leaf
// selection.

type leafShape int

const (
	shapeScan leafShape = iota
	shapeSeek
	shapePrefix
	shapeRange
)

// plannedLeaf is one candidate (or the chosen) access path.
type plannedLeaf struct {
	shape leafShape
	index qrep.IndexInfo
	// eqVals holds the consumed EQ literals, one per leading
	// column position.
	eqVals [][]byte
	// bound is the trailing range-shaped term (shapeRange only).
	bound    qrep.Term
	hasBound bool
	// consumed holds indexes into the query's term slice for every
	// term this leaf fully realizes — dropped from residual
	// evaluation. A term is consumed only when the leaf's entry
	// interval expresses its exact semantics (Inv-QB1).
	consumed []int
	// needDedup reports that some multi-valued column is NOT fixed
	// by an EQ term: the leaf can then yield one entry per element
	// for a single row, and the executor dedups by PK (Inv-QB4).
	needDedup bool
	// covers reports rule 3(b): key + covering columns satisfy
	// every column the query's terms touch (CoverValue trivially
	// does).
	covers bool
}

// planQuery chooses the access path for the query's top-level
// conjunction over the handle's declared ColumnIndexes. Rule 1's
// partition is implicit: only leaf terms with EQ/range shapes on
// declared columns are consumable; Or terms and opaque filters
// always evaluate residually at this stage.
func planQuery(terms []qrep.Term, infos []qrep.IndexInfo) plannedLeaf {
	touched := touchedColumns(terms)
	entailed := entailedMultiColumns(terms)
	var cands []plannedLeaf
	for _, info := range infos {
		// Rule 7: Where-partial indexes are never planner-eligible
		// — the entry set excludes rows the query may want, and no
		// sound eligibility test over an opaque predicate exists.
		if info.Partial {
			continue
		}
		if leaf, ok := matchIndex(terms, info, touched, entailed); ok {
			cands = append(cands, leaf)
		}
	}
	if len(cands) == 0 {
		return plannedLeaf{shape: shapeScan}
	}
	// Rule 3 scoring: (a) most terms consumed, (b) covering,
	// (c) unique over non-unique, (d) index name — the total order
	// that makes the choice deterministic (Inv-QB5).
	slices.SortFunc(cands, func(a, b plannedLeaf) int {
		if d := len(b.consumed) - len(a.consumed); d != 0 {
			return d
		}
		if a.covers != b.covers {
			if a.covers {
				return -1
			}
			return 1
		}
		if a.index.Unique != b.index.Unique {
			if a.index.Unique {
				return -1
			}
			return 1
		}
		return strings.Compare(a.index.Name, b.index.Name)
	})
	return cands[0]
}

// entailedMultiColumns collects the multi columns whose element
// existence the query entails: a TOP-LEVEL Contains/ContainsRange
// term (consumed or residual) is false for every empty-element
// row, so an index omitting those rows loses nothing the query
// wants. An Or-nested Contains does NOT entail — a disjunct may be
// false for a row the other group matches.
func entailedMultiColumns(terms []qrep.Term) map[string]struct{} {
	out := make(map[string]struct{})
	for _, t := range terms {
		if t.Kind == qrep.KindContains || t.Kind == qrep.KindContainsRange {
			out[t.ColumnName] = struct{}{}
		}
	}
	return out
}

// matchIndex applies rule 2 to one index: the longest leading
// column prefix satisfiable by EQ terms (Eq / Contains), optionally
// extended by ONE range-shaped term on the next column.
func matchIndex(terms []qrep.Term, info qrep.IndexInfo, touched, entailed map[string]struct{}) (plannedLeaf, bool) {
	leaf := plannedLeaf{index: info}
	used := make([]bool, len(terms))
	pos := 0
	for ; pos < len(info.KeyCols); pos++ {
		found := -1
		for ti := range terms {
			if used[ti] {
				continue
			}
			k := terms[ti].Kind
			if (k == qrep.KindEq || k == qrep.KindContains) && terms[ti].ColumnName == info.KeyCols[pos].Name {
				found = ti
				break
			}
		}
		if found < 0 {
			break
		}
		used[found] = true
		leaf.consumed = append(leaf.consumed, found)
		leaf.eqVals = append(leaf.eqVals, terms[found].Lo)
	}
	if pos < len(info.KeyCols) {
		for ti := range terms {
			if used[ti] || terms[ti].ColumnName != info.KeyCols[pos].Name {
				continue
			}
			switch terms[ti].Kind {
			case qrep.KindLt, qrep.KindLte, qrep.KindGt, qrep.KindGte,
				qrep.KindBetween, qrep.KindHasPrefix, qrep.KindContainsRange:
				leaf.bound = terms[ti]
				leaf.hasBound = true
				leaf.consumed = append(leaf.consumed, ti)
			}
			if leaf.hasBound {
				break
			}
		}
	}
	if len(leaf.consumed) == 0 {
		return plannedLeaf{}, false
	}
	// A row whose MultiColumn accessor returns an empty slice has
	// NO entries in any index containing that column (an empty
	// column sequence empties the Cartesian product —
	// typed-columns.md Inv-TC4): the index is row-partial at
	// element granularity. It is a sound access path only when
	// every multi column's element existence is ENTAILED by the
	// query — consumed by the leaf (Contains in the EQ prefix,
	// ContainsRange as the bound) or covered by a top-level
	// Contains/ContainsRange term evaluating residually; either
	// way the rows the index omits are rows the query rejects.
	// Otherwise the leaf silently loses empty-element rows a scan
	// would return (Inv-QB1) — rule 7's exclusion class.
	for j := 0; j < len(info.KeyCols); j++ {
		if !info.KeyCols[j].Multi {
			continue
		}
		consumedHere := j < pos || (j == pos && leaf.hasBound)
		if _, ok := entailed[info.KeyCols[j].Name]; !ok && !consumedHere {
			return plannedLeaf{}, false
		}
	}
	switch {
	case pos == len(info.KeyCols):
		leaf.shape = shapeSeek
	case leaf.hasBound:
		leaf.shape = shapeRange
	default:
		leaf.shape = shapePrefix
	}
	// A multi-valued column fixed by a Contains EQ in the prefix
	// yields exactly one entry per matching row; any multi column
	// at or past the EQ prefix — the ContainsRange bound column
	// and residual-entailed tails alike — can yield one entry per
	// element (Inv-QB4).
	for j := pos; j < len(info.KeyCols); j++ {
		if info.KeyCols[j].Multi {
			leaf.needDedup = true
			break
		}
	}
	leaf.covers = coversTouched(info, touched)
	return leaf, true
}

// touchedColumns collects every column name the query's terms
// reference, disjuncts included — rule 3(b)'s "every column the
// query touches" (this stage has no Select or OrderBy).
func touchedColumns(terms []qrep.Term) map[string]struct{} {
	touched := make(map[string]struct{})
	var walk func(ts []qrep.Term)
	walk = func(ts []qrep.Term) {
		for _, t := range ts {
			if t.Kind == qrep.KindOr {
				for _, g := range t.Disjuncts {
					walk(g)
				}
				continue
			}
			if t.ColumnName != "" {
				touched[t.ColumnName] = struct{}{}
			}
		}
	}
	walk(terms)
	return touched
}

func coversTouched(info qrep.IndexInfo, touched map[string]struct{}) bool {
	if info.CoverValue {
		return true
	}
	carried := make(map[string]struct{}, len(info.KeyCols)+len(info.Covering))
	for _, c := range info.KeyCols {
		carried[c.Name] = struct{}{}
	}
	for _, c := range info.Covering {
		carried[c] = struct{}{}
	}
	for name := range touched {
		if _, ok := carried[name]; !ok {
			return false
		}
	}
	return true
}

// residualTerms returns the terms the chosen leaf does not consume.
func residualTerms(terms []qrep.Term, consumed []int) []qrep.Term {
	if len(consumed) == 0 {
		return terms
	}
	drop := make(map[int]struct{}, len(consumed))
	for _, i := range consumed {
		drop[i] = struct{}{}
	}
	out := make([]qrep.Term, 0, len(terms)-len(consumed))
	for i, t := range terms {
		if _, ok := drop[i]; !ok {
			out = append(out, t)
		}
	}
	return out
}

// rangeBounds compiles the trailing bound term into byte Range
// bounds (query-builder.md §Byte-surface requirements): intervals
// are constructed at the VALUE level — an EQ-prefix group is
// closed from above by the value-level successor X||0x00 as the
// bound column — so the builder never touches the NUL-escape
// encoding.
func rangeBounds(eq [][]byte, t qrep.Term) (start, end [][]byte) {
	switch t.Kind {
	case qrep.KindLt:
		return openIfEmpty(eq), appendCol(eq, t.Lo)
	case qrep.KindLte:
		return openIfEmpty(eq), appendCol(eq, valueSuccessor(t.Lo))
	case qrep.KindGt:
		return appendCol(eq, valueSuccessor(t.Lo)), groupClose(eq)
	case qrep.KindGte:
		return appendCol(eq, t.Lo), groupClose(eq)
	case qrep.KindBetween, qrep.KindContainsRange:
		return appendCol(eq, t.Lo), appendCol(eq, t.Hi)
	case qrep.KindHasPrefix:
		if ps := prefixSuccessor(t.Lo); ps != nil {
			return appendCol(eq, t.Lo), appendCol(eq, ps)
		}
		// All-0xFF (or empty) literal: no value successor exists —
		// the interval extends to the end of the EQ-prefix group.
		return appendCol(eq, t.Lo), groupClose(eq)
	}
	// Unreachable: matchIndex admits only the kinds above as
	// bounds. A new range kind missed here must fail loud.
	panic("gmdb/query: rangeBounds: unhandled bound kind")
}

// openIfEmpty maps an empty EQ prefix to the open bound (nil).
func openIfEmpty(eq [][]byte) [][]byte {
	if len(eq) == 0 {
		return nil
	}
	return eq
}

// appendCol returns eq extended by one bound column value, without
// aliasing the input slice.
func appendCol(eq [][]byte, v []byte) [][]byte {
	out := make([][]byte, len(eq)+1)
	copy(out, eq)
	out[len(eq)] = v
	return out
}

// valueSuccessor returns the smallest byte string strictly greater
// than v in lex order: v || 0x00.
func valueSuccessor(v []byte) []byte {
	out := make([]byte, len(v)+1)
	copy(out, v)
	return out
}

// groupClose returns the exclusive upper bound of the EQ-prefix
// group: the same tuple with the last column's value-level
// successor. nil (open) for an empty prefix.
func groupClose(eq [][]byte) [][]byte {
	if len(eq) == 0 {
		return nil
	}
	out := make([][]byte, len(eq))
	copy(out, eq[:len(eq)-1])
	out[len(eq)-1] = valueSuccessor(eq[len(eq)-1])
	return out
}

// prefixSuccessor returns the smallest byte string strictly
// greater than every string having p as a prefix — p with its
// rightmost non-0xFF byte incremented and the tail dropped. nil
// when no such string exists (empty or all-0xFF p).
func prefixSuccessor(p []byte) []byte {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] != 0xFF {
			out := make([]byte, i+1)
			copy(out, p[:i+1])
			out[i]++
			return out
		}
	}
	return nil
}

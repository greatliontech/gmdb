package query

import "fmt"

// Plan is a query's chosen execution shape, returned by Explain as
// a value without executing (query-builder.md §Query surface) —
// the surface plan-pinning tests and operators use. Node kinds
// follow the spec's taxonomy (§Plan nodes and the ordering
// property); only the kinds with a landed implementation appear.
type Plan struct {
	Root PlanNode
}

func (p Plan) String() string {
	if p.Root == nil {
		return "<nil>"
	}
	return p.Root.String()
}

// PlanNode is one node of a Plan tree. The interface is sealed —
// plan trees are built by the planner, only inspected by callers.
type PlanNode interface {
	fmt.Stringer
	planNode()
}

// ValueRoute is a plan leaf's value-acquisition route
// (query-builder.md §Covering-aware execution). Explain derives it
// from the handle's open-time declarations; execution re-derives
// against the LIVE declaration (Inv-QB3), so a same-tx Rebuild can
// make the executed route differ — results are identical either
// way (the routes are pure read strategies). With a Select the
// route names what Rows() would do; All() on the same query never
// serves index-only and takes the declaration's row route
// (cover-value, back-lookup, or row bytes).
type ValueRoute uint8

const (
	// ValuesRowBytes: the entry value bytes ARE the row bytes (a
	// non-covering index) and decode directly.
	ValuesRowBytes ValueRoute = iota
	// ValuesBackLookup: the entry carries a covering tuple the
	// query does not consume; rows fetch from the row keyspace.
	ValuesBackLookup
	// ValuesCoverValue: full-row covering — V decodes from the
	// entry's covering bytes, no row read.
	ValuesCoverValue
	// ValuesEntry: index-only — every needed slot resolves from
	// entry key/covering bytes; neither rows nor V materialize.
	ValuesEntry
)

func (r ValueRoute) String() string {
	switch r {
	case ValuesRowBytes:
		return "row-bytes"
	case ValuesBackLookup:
		return "back-lookup"
	case ValuesCoverValue:
		return "cover-value"
	case ValuesEntry:
		return "entry"
	}
	return "?"
}

// Scan is the full-scan leaf over the row keyspace: every term and
// filter evaluates residually.
type Scan struct{}

func (Scan) planNode()      {}
func (Scan) String() string { return "Scan" }

// IndexSeek is the exact-match leaf: every declared column of the
// index is consumed by an EQ-shaped term (byte Lookup).
type IndexSeek struct {
	// Index is the ColumnIndex declaration name.
	Index string
	// Values is the leaf's value-acquisition route.
	Values ValueRoute
}

func (IndexSeek) planNode() {}
func (n IndexSeek) String() string {
	return fmt.Sprintf("IndexSeek(%s, values=%s)", n.Index, n.Values)
}

// IndexPrefix is the leading-EQ-prefix leaf with no trailing bound
// (byte Prefix).
type IndexPrefix struct {
	Index string
	// PrefixLen is the number of leading columns consumed by EQ
	// terms (1 ≤ PrefixLen < the index's column count).
	PrefixLen int
	Values    ValueRoute
}

func (IndexPrefix) planNode() {}
func (n IndexPrefix) String() string {
	return fmt.Sprintf("IndexPrefix(%s, prefix=%d, values=%s)", n.Index, n.PrefixLen, n.Values)
}

// IndexRange is the leaf consuming a (possibly empty) leading-EQ
// prefix plus ONE range-shaped term on the next column (byte Range
// partial-tuple prefix-bounds).
type IndexRange struct {
	Index string
	// PrefixLen counts only the EQ-consumed leading columns; the
	// bound column at position PrefixLen is not included.
	PrefixLen int
	Values    ValueRoute
}

func (IndexRange) planNode() {}
func (n IndexRange) String() string {
	return fmt.Sprintf("IndexRange(%s, prefix=%d, values=%s)", n.Index, n.PrefixLen, n.Values)
}

// Project is the projection transform (query-builder.md §Plan
// nodes): present when the query carries a Select; Rows() decodes
// its columns from the input's slots.
type Project struct {
	Input PlanNode
	// Columns counts the selected columns.
	Columns int
}

func (Project) planNode() {}
func (n Project) String() string {
	return fmt.Sprintf("Project(cols=%d) <- %s", n.Columns, n.Input)
}

// ResidualFilter wraps the leaf when any terms stay unconsumed or
// opaque filters are present: they evaluate residually over the
// leaf's rows (Inv-QB2 / Inv-QB7).
type ResidualFilter struct {
	Input PlanNode
	// Terms counts residual structured terms; Filters counts
	// opaque Filter funcs.
	Terms   int
	Filters int
}

func (ResidualFilter) planNode() {}
func (n ResidualFilter) String() string {
	return fmt.Sprintf("ResidualFilter(terms=%d, filters=%d) <- %s", n.Terms, n.Filters, n.Input)
}

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
}

func (IndexSeek) planNode() {}
func (n IndexSeek) String() string {
	return fmt.Sprintf("IndexSeek(%s)", n.Index)
}

// IndexPrefix is the leading-EQ-prefix leaf with no trailing bound
// (byte Prefix).
type IndexPrefix struct {
	Index string
	// PrefixLen is the number of leading columns consumed by EQ
	// terms (1 ≤ PrefixLen < the index's column count).
	PrefixLen int
}

func (IndexPrefix) planNode() {}
func (n IndexPrefix) String() string {
	return fmt.Sprintf("IndexPrefix(%s, prefix=%d)", n.Index, n.PrefixLen)
}

// IndexRange is the leaf consuming a (possibly empty) leading-EQ
// prefix plus ONE range-shaped term on the next column (byte Range
// partial-tuple prefix-bounds).
type IndexRange struct {
	Index string
	// PrefixLen counts only the EQ-consumed leading columns; the
	// bound column at position PrefixLen is not included.
	PrefixLen int
}

func (IndexRange) planNode() {}
func (n IndexRange) String() string {
	return fmt.Sprintf("IndexRange(%s, prefix=%d)", n.Index, n.PrefixLen)
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

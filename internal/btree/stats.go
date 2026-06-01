package btree

// SplitMergeRecorder is an optional capability a PageWriter may
// implement to receive node split / merge notifications, backing the
// root package's TxStats.Splits / TxStats.Merges (api-surface.md
// §Statistics). *pager.Pager satisfies it; test PageWriters that do not
// care simply omit the methods, in which case recordSplit / recordMerge
// are no-ops (the type assertion fails).
//
// A "split" is one node (leaf or branch) divided into two — counted once
// per new sibling created on the insert path. A "merge" is two nodes
// combined into one — counted once per node freed by the delete-side
// rebalance (a redistribute, which moves entries without freeing a node,
// is not a merge).
type SplitMergeRecorder interface {
	RecordSplit()
	RecordMerge()
}

func recordSplit(pw PageWriter) {
	if r, ok := pw.(SplitMergeRecorder); ok {
		r.RecordSplit()
	}
}

func recordMerge(pw PageWriter) {
	if r, ok := pw.(SplitMergeRecorder); ok {
		r.RecordMerge()
	}
}

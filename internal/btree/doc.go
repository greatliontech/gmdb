// Package btree implements the B+tree algorithms gmdb uses for
// every keyspace (single-value, set, index). The package operates
// on raw page byte slices via the page package codecs and a small
// PageReader / PageWriter interface that *pager.Pager satisfies —
// keeping btree free of pager-internal concerns (slab management,
// transaction state) so each sub-tree (user keyspace, index
// keyspace, per-keyspace index registry) can be driven against the
// same primitives.
//
// The package provides, over raw page bytes:
//
//   - Read-only descent: Get(rootID, key) ⇒ value via recursive
//     descent and leaf binary search (page.LeafReader.SearchLeaf).
//   - Insert + split: copy-on-write from leaf to root with
//     prefix-truncated separators for new branch entries.
//   - Delete + merge/redistribute, governed by MergeThreshold.
//   - A variable-size restart-group leaf format (page.LeafReader /
//     LeafBuilder / LeafIter); Validate runs at the pager-resolve
//     boundary per internal/page/leaf.go.
//   - A bidirectional cursor on LeafIter + a generation counter per
//     transactions.md §Cursor State Machine.
//   - Overflow inline-value support: a value larger than a leaf
//     page is written as an overflow run.
//
// Atomic-state contract. btree functions take immutable snapshots
// of (cfg, rootID); write operations return the NEW rootID after
// CoW propagation. The pager + tx owns the actual root storage and
// the page-buffer lifetime; btree never holds a *pager.Pager
// reference past its single-call entry point.
package btree

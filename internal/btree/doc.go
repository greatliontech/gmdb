// Package btree implements the B+tree algorithms gmdb uses for
// every keyspace (single-value, set, index). The package operates
// on raw page byte slices via the page package codecs and a small
// PageReader / PageWriter interface that *pager.Pager satisfies —
// keeping btree free of pager-internal concerns (slab management,
// transaction state) so each sub-tree (user keyspace, index
// keyspace, per-keyspace index registry) can be driven against the
// same primitives.
//
// Chunk 4 surface, in landing order:
//
//   - 4.3 (this) — read-only descent: Get(rootID, key) ⇒ value.
//     PageReader interface, recursive descent from root through
//     branches to a leaf, leaf binary search via page.LeafLookup.
//   - 4.4 — Insert + split: CoW from leaf to root, prefix-truncated
//     separator computation for new branch entries.
//   - 4.5 — Delete + merge/redistribute. MergeThreshold option.
//   - 4.6 — Cursor: forward Next, reverse Prev with group cache,
//     key reconstruction buffer, Cursor.Delete state machine per
//     transactions.md §Cursor State Machine.
//   - 4.7 — Overflow inline-value support: Put with values >
//     leaf-page capacity writes an overflow run.
//
// Atomic-state contract. btree functions take immutable snapshots
// of (cfg, rootID); write operations return the NEW rootID after
// CoW propagation. The pager + tx owns the actual root storage and
// the slab buffer lifetime; btree never holds a *pager.Pager
// reference past its single-call entry point.
package btree

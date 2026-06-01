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
//   - 4.3 — read-only descent: Get(rootID, key) ⇒ value. PageReader
//     interface, recursive descent from root through branches to a
//     leaf, leaf binary search via page.LeafReader.SearchLeaf.
//   - 4.4 — Insert + split: CoW from leaf to root, prefix-truncated
//     separator computation for new branch entries.
//   - 4.5 — Delete + merge/redistribute. MergeThreshold option.
//   - 4.6 — Leaf format reset + cursor.
//     · α — spec amend (variable-size restart groups +
//     uncompressed-variant + LeafIter + gen counter).
//     · β — page-package rewrite: LeafReader / LeafBuilder /
//     LeafIter; old encoders (DecodeLeaf / EncodeLeaf /
//     LeafLookup / LeafEncodedSize / LeafRestartInterval) dropped.
//     · γ (this) — btree port onto the new leaf surface: Put,
//     Delete, Get, merge/redistribute all migrated to
//     LeafReader + LeafBuilder. Validate runs at the pager-
//     resolve boundary per the chunk-4.6β leaf-doc contract.
//     · δ — bidirectional cursor on LeafIter + generation counter
//     per transactions.md §Cursor State Machine.
//   - 4.7 — Overflow inline-value support: Put with values >
//     leaf-page capacity writes an overflow run.
//
// Atomic-state contract. btree functions take immutable snapshots
// of (cfg, rootID); write operations return the NEW rootID after
// CoW propagation. The pager + tx owns the actual root storage and
// the page-buffer lifetime; btree never holds a *pager.Pager
// reference past its single-call entry point.
package btree

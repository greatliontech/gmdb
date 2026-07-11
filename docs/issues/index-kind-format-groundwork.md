# Index-kind format groundwork: registry entry v2 + IndexDecl.Kind

Lands: 7

Spec-amend rider: requirements for the chunk that makes the
index formats kind-extensible, so future index kinds (full-text,
vector) are format changes already absorbed rather than
migrations. Ratified 2026-07-11 as a pre-v1 clean break while no
installed base exists. The requirements below are the
2026-07-11 architecture audit's findings; the chunk designs the
actual grammar under Spec-first against
`indexing.md §Storage Layout` + §Drift Guard.

## Requirements

- The registry entry (`internal/indexing/registry.go`) carries
  no kind discriminator, exactly one `Root uint64` and one
  `Count uint64`, and `Padding [7]byte` is invariant-zero
  (pinned by test). A second index kind needs: a kind tag, and
  room for per-kind metadata — full-text: a corpus-stats head
  (doc count, Σ doc length, df-table root) beside the postings
  root; vector: a centroid-tree root + config (dim, metric,
  partition params). Entry v2 must give kinds a
  length-prefixed payload without per-kind format churn.
- `IndexDecl` has no `Kind` field; the schema hash folds
  `Name | Columns | Covering | Unique` only. v2 folds kind +
  kind-params into the fingerprint (drift guard catches kind
  changes). Changes every stored `SchemaHash` — part of the
  same clean break.
- In-memory ripple: `pinnedIndex` `{decl, schemaHash, root,
  count}`, `snapshotIndexes` / `restoreIndexes`, and
  `flushIndexRegistry` all assume a single `(root, count)`
  pair per index; v2 shapes them to carry the kind payload.
- Non-goals for the groundwork chunk: no second kind is
  implemented; the write-path maintenance seam and ranked read
  surfaces stay untouched (see
  `docs/issues/index-background-maintenance-hook.md` for the
  structural gap deliberately not addressed here).

## Provenance

2026-07-11 architecture audit (typed-tier severance, root
cluster map, index-kind assumptions inventory), run during the
query-builder plan's docs change set.

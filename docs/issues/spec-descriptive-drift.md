# Spec descriptive drift: clauses describing mechanisms the code doesn't use

Lands: 22

Batch of spec-text corrections where the code is behaviorally right (or
the divergence is externally equivalent) and the spec misdescribes the
mechanism or over-specifies code shapes. Each was surfaced as a
spec-amend candidate in the 2026-07-10 audit; default disposition is to
align the spec with reality and delete code-shape content per the
artifact-homes rule. Behavioral spec-amends ride their fix chunks and
are NOT in this batch.

## Items

- **pager-slab.md §Step 0**: says the new meta payload is constructed
  in step 0; the code correctly builds it after step 2
  (`commit.go:145-153`) because durability.md §Anchoring's
  no-forward-promise requires it. Move the bullet; durability.md is the
  governing tier.
- **pager-slab.md §Roles struct sketch**: `bufPool *sync.Pool` and the
  exact Pager field list have drifted far from the code; code shapes
  don't belong in a spec — remove the sketch.
- **durability.md §Checkpoint mechanics step 3**: prescribes an
  unconditional read-bump-pwrite; the code skips the pwrite when the
  meta is already self-durable (`checkpoint.go:144-149`). Bless the
  skip (poison + errseq cover the fsync-error trap) or mandate the
  rewrite; pick one and align.
- **indexing.md**: three clauses describe mechanisms the code
  (defensibly) doesn't use — rebuild Stats visibility ("returns the
  old registry entry's count" vs implemented read-your-writes), per-op
  registry Count updates (deferred to Commit's `flushIndexRegistry`),
  and rebuild retirement via `tx.retiredPages` (implemented via
  `FreeSubtree` into the loose pool). Also: covering-shape change
  through a cached handle is unspecified (§Handle Invalidation covers
  column-arity only).
- **keyspaces.md**: intro + Kind=2 machinery describe per-index
  descriptor rows that are never created (registry is a sub-tree at
  `IndexRegistryRoot`); the RestartGroupTarget>255 Open failure is
  spec'd as ErrInvalidOptions but surfaced as ErrCorrupted (correct,
  since only on-disk corruption can produce it — spec should say so).
- **checksums.md §Structural and Allocation Bounds**: scope the
  error-not-crash contract explicitly over CopyTo/Compact walks (the
  code fix lands earlier; the scope sentence lands here if not already
  amended there).
- **page-formats.md §Compressed Leaf**: the persisted
  `min(2*RestartGroupTarget, 255)` group-growth cap from in-place
  inserts (`leaf_splice.go:317-320`) is observable on-disk state and
  belongs in the spec.
- **page-formats.md contiguous-stream invariant (≈line 87)**: the
  from-clause grounds the invariant in a variant-generic "streaming
  iterator … never re-consult[s] the lookup tables mid-stream", but the
  uncompressed iterator is table-driven on every op (per §Cursor
  Iteration's own O(1)-via-table clause). The property stands (the
  compressed streaming iterator, the compressed splice continuation
  walks at leaf_splice.go:345/648, and write-side DataEnd placement
  depend on it); reword the from/violation prose to name those actual
  continuation consumers. Raised by the chunk-1 change-set review.
- **cross-process.md**: the future-stamp invariant's darwin/freebsd
  rationale is factually wrong (both clocks are boot-relative,
  kernel-wide), and the spec presents macOS/FreeBSD as supported while
  `internal/lock/mmap_other.go` makes the lock file Linux-only. Align
  the platform claims (the boot-epoch behavioral amend rides its own
  chunk).

## Provenance

2026-07-10 defect audit; spec-amend candidates consolidated from all
nine subsystem reviewers. Behavioral riders live in the issue of the
chunk that implements them.

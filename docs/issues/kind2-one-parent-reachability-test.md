# Test gap: Kind=2 one-parent-reachability uniqueness asserted only in the "no top-level pollution" direction

**Lands:** chunk 7.8 (when `DropIndex` / `RebuildIndex` / three-
subtree retirement code paths first manipulate
`IndexRegistryRoot` copies — the natural point where a regression
could break the per-keyspace uniqueness).

## Problem

The chunk-7.1 indexing.md entailed invariant states that every
engine-internal Kind=2 keyspace descriptor is reachable via
**exactly one** user-keyspace's index registry sub-tree — never
via the top-level keyspace B+tree, and never via two distinct
parent keyspaces.

Chunk-7.5 enforces this in one direction:
`TestCreateKeyspaceWithIndexDoesNotPolluteListKeyspaces` asserts
that `ListKeyspaces` shows only the parent keyspace, not any
per-index internals. Together with the chunk-5.4 forge test
`TestListKeyspacesFiltersKindIndexInternal`, this fully covers
the "never via the top-level keyspace B+tree" portion.

The "exactly one parent" portion is **structurally** guaranteed
by chunk-7.5's per-keyspace `IndexRegistryRoot` allocation: each
`CreateKeyspace` allocates its own registry sub-tree, with no
shared state between parents. There is no test that asserts this
uniqueness — a future regression in which two descriptors held
the same `IndexRegistryRoot` page ID (e.g. a bad copy in a refactor
of `DeleteKeyspace`'s three-subtree retirement at chunk 7.8) would
silently violate the invariant without surfacing.

## Acceptance

Add a regression test at chunk 7.8 (or earlier) that:

1. Creates two user keyspaces, each with one or more indexes.
2. Walks the keyspace B+tree, collecting every descriptor's
   `IndexRegistryRoot` field.
3. Asserts that no two descriptors share the same non-zero
   `IndexRegistryRoot` page ID (no two parents reference the same
   registry sub-tree root).

Strengthen at chunk 11 `Check(CheckIndexes)` with a full walk
that asserts the same property across all on-disk descriptors —
not just same-tx-created ones.

When this issue closes, the load-bearing rationale moves inline
into the chunk-7.8 test (or the chunk-11 `Check()` invariant) and
this file is deleted per the no-cite invariant.

## Notes

Surfaced by the chunk-7.5 Round-1 adversarial review (L-2). The
indexing.md §Invariants entailed invariant added at chunk-7.1
remains correctly **recorded**; this issue is about the test gap
in the **enforced** half of the chunk-7.5 spec-tier promotion.

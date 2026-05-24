# `NumKeyspaces` spec semantics undefined

**Lands:** chunk 7 (when Kind=2 engine-internal keyspaces are first
created and the leaf-count vs user-visible-count distinction becomes
observable through user-facing stats) — or earlier if a spec amend is
preferred to lock the chunk-5.4 implementation choice.

## Problem

`docs/specs/keyspaces.md §Keyspace Descriptor` references
`NumKeyspaces` (in the meta-page wiring) but does not specify whether
it counts:

- (a) **all leaf entries in the keyspace B+tree**, including Kind=2
  engine-internal entries (per-index storage etc.), or
- (b) **only user-visible keyspaces** (Kind=0 + Kind=1), filtering out
  Kind=2.

Chunk 5.4 implements (a) — `tx.numKeyspaces++` on every
`storeDescriptor` regardless of Kind. The chunk-5.4 test
`TestListKeyspacesFiltersKindIndexInternal` mirrors this by bumping
`tx.numKeyspaces++` after forging a Kind=2 entry. Inv-C of chunk 5.4
states "`numKeyspaces` equals the count of leaf entries in the
keyspace B+tree" — incl Kind=2.

The chunk-5.4 adversarial review flagged this as a spec-amend
candidate: the implementation is internally consistent, but the spec
silence allows future drift if a user-visible `DBStats.NumKeyspaces`
field surfaces (chunks 11+) and someone reads it as "user keyspaces."

## Decision

Resolve before any user-visible API surfaces NumKeyspaces:

1. **Amend keyspaces.md** to make the leaf-count semantics explicit
   (Inv-C of chunk 5.4 becomes a clause-explicit invariant in the
   spec) — minimal change matching current code.
2. **Conform code to user-visible-only** by splitting into two
   fields: meta carries the full leaf count (for B+tree audit), and
   a separate counter (or computed-on-demand via cursor walk) gives
   the user-visible count.

Default if undecided: amend spec (option 1) — Kind=2 entries are
already filtered by `ListKeyspaces`, so the user-visible-count semantic
is recoverable via cursor walk without a separate counter.

When this issue closes, the load-bearing rationale moves inline into
`keyspaces.md §Keyspace Descriptor` (or §Invariants) and this file is
deleted per the no-cite invariant.

## Notes

Surfaced by the chunk-5.4 Round 1 adversarial review (L2 spec-amend
candidate). Filed-and-deferred at 5.4 close-out because the choice
has no chunk-5.4-observable impact; the chunk that first surfaces
NumKeyspaces in a user API (or one that needs the count for index
accounting) is the natural decision point.

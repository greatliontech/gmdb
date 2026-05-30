# `btree.PageWriter` names pager internals; interface mirrors `*pager.Pager`

**Lands:** condition — proactive burn-down; pull when next touching the
btree↔pager boundary. Internal-only (both packages are `internal/`),
not breaking to external callers.

## Problem

`internal/btree`'s mutator interface embeds *pager-internal vocabulary*
in `btree`'s own surface:

- `internal/btree/put.go:20` — `PageWriter` declares `CoW(srcID, dstID
  uint64)` and `AllocSlab(id uint64)`. "CoW" is a pager MVCC mechanic;
  "slab" is pager bufpool terminology. `btree` has no business naming
  either.
- The method doc-comments embed pager internals `btree` does not own:
  "loose → bitmap → RPL reclamation → file extension" (`put.go:23`),
  "prior-tx pages enter the RPL at commit" (`put.go:41`), and a
  byte-ownership note tied to `pager-slab.md` (`btree.go:26`).

This is **naming/doc coupling, not behavioral coupling** — verified:
`btree` never implements or branches on MVCC; it treats `CoW` as an
opaque "clone src→dst, hand me a writable buffer" primitive (the
dirty-map, idempotent re-CoW, bufpool, and RPL bookkeeping all live in
`internal/pager/pager.go:709-740`). So the abstraction is *behaviorally*
clean but its *names* make `PageWriter` a structural echo of
`*pager.Pager`'s exact signatures — a pager method rename ripples into
`btree`.

Two related minor points from the same audit:

- **Interface-is-an-echo-of-concrete.** There is no write-path adapter;
  the root passes the concrete `*pager.Pager` straight into
  `btree.Put`/`Delete`/`FreeSubtree`. Cheap glue (good) but only
  possible because `PageWriter`'s signatures were shaped to match the
  pager's — which is *why* the naming leak above exists.
- **Three overlapping consumer interfaces** describe the pager's
  page-store surface: `PageReader` (`btree.go:19`), `PageWriter`
  (`put.go:20`), and `bulkPageWriter` (`bulkload.go:26`). The bulk one
  is justified (slab-bypass), but the surface is now fragmented across
  three declarations.

## Resolution

Rename the buffer-lifecycle primitives to storage-neutral terms in the
`btree` interface (`CoW` → `CopyPage`, `AllocSlab` → `ZeroPage` /
`AllocBuffer`) and scrub pager-internal references from `btree`'s
doc-comments — describe the contract in `btree`'s own terms ("give me
the bytes of page N", "give me a writable copy of page N", "give me a
fresh zeroed page"). Zero behavioral change. Optionally consolidate the
three consumer interfaces, or document why the bulk path is a
deliberate separate contract.

## Notes

Surfaced during the 2026-05-30 architecture/factoring audit
(internal-package boundary pass). The boundary was otherwise graded
strong — bytes vs algorithm vs allocation/MVCC is cleanly split and
enforced by the acyclic import graph; this is the one naming flaw in an
otherwise clean decomposition.

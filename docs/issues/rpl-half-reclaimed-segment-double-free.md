# Crash-image half-reclaimed RPL segment: re-reclamation double-frees a live page

Lands: 7

## Finding

**[H] A crash between bitmap-page writebacks leaves a segment whose
entries are already free while the segment survives in the recovered
chain; later reclamation frees those pages a second time — after they
may have been re-allocated into the live tree.**
`internal/pager/rplwalk.go:224-228` (reclaimed-boundary test checks only
the segment page's own free bit, never the entries),
`internal/pager/init.go:755-801` (`rebuildRPLChain` inherits it),
`internal/pager/freespace.go:471-538` (`reclaimRPL` unconditionally
`Set`s every entry of an in-chain segment).

Failure (reachable in every SyncMode): reclamation of tail segment S
sets entry bits and S's own bit on *different* bitmap pages (one 4 KiB
bitmap page covers 32 768 page-bits); commit step 1 pwrites both; crash
persists the entry-bit page but not S's-bit page while recovery selects
the pre-reclamation meta. Reopen: `rebuildRPLChain` re-includes S (its
page-bit is clear); the in-memory bitmap shows the entries free;
`AllocPage` hands one out; a commit publishes a tree referencing it;
later `reclaimRPL` reaches S and marks the live page free → next
`AllocPage` hands the same page to a second owner → silent corruption
(`ErrBadPageChecksum` at best, structurally-valid wrong bytes at
worst). `check.go:800-807` names this exact state (`FreeAndPending`,
"will be set free a second time…") as a CheckError, but the runtime
Open/recovery path neither detects nor neutralizes it. free-space.md's
"Why the bound is sufficient" argument covers only the recovered
tree's references, not pages re-allocated after recovery.

## Fix direction

Neutralize at Open: when rebuilding the chain, detect entries of an
in-chain segment already free in the adopted bitmap (the FreeAndPending
state) and reconcile — drop/compact the stale entries (or re-mark them
pending) before any allocation is served. The chosen invariant lands in
free-space.md's crash-ordering section (spec-amend rider, surfaced in
the audit spec-amend list). Regression: crash-harness image persisting
the entry-bit bitmap page but not the segment-page-bit page, then
post-recovery allocate → commit → forced reclamation; assert no
double-allocation and Check clean.

## Provenance

2026-07-10 defect audit; pager/commit reviewer. Existing crash-harness
tests verify images read-only and never drive post-recovery allocation
followed by re-reclamation.

# Bitmap-leak reclamation races concurrent commits and can free live tree pages (in-process and cross-process)

**Lands:** audit-burndown-2026-07 chunk 13.

**Severity:** [H] — demonstrated in-process (default options): 6
live-committed pages classified leaked, 5 ReachableButFree after
reclamation. [M] cross-process variant: overlapping maintenance passes
(start-staggered only, `lock.go:641`) can FreeLeakedPage a page a
peer already freed and a writer re-allocated.

**Source:** 2026-07-04 full-codebase audit (bulkload/maintenance +
concurrency auditors; shared fix).

**Governing spec:** `docs/specs/background-maintenance.md` §Bitmap
Leak Reclamation — its safety invariant ("the leaked-page set is a
function of the snapshot's tree") is not what the code computes.

## Problem

Detection derives the leaked set from the snapshot tree at TxnID T but
the **live** bitmap: `snapshotBitmap` (`check.go:977-991`) copies mmap
bytes after the walk; the bitmap has no MVCC and a concurrent commit
T+1 pwrites it in place. A page free at T and allocated by T+1
classifies `!reach && !free && !pending` → leaked (`check.go:730-735`)
→ `FreeLeakedPage`d (`internal/pager/freespace.go:412-427` — only
rejects already-free ids; its godoc states the exclusivity
precondition the caller doesn't hold, while `maintReclaimLeaks`'s
comment at `maintenance.go:234-289` claims none is needed — that
argument covers only pages already leaked at snapshot time). T+1's
new RPL segment page is misclassified the same way. Cross-process:
`TryClaimMaintenance` staggers pass starts, not durations; a slow
pass's reclamation commits against a world a faster peer changed.

## Fix direction

Make reclamation validate its detection snapshot inside the write tx:
proceed only if current meta TxnID == detection snapshot TxnID (else
discard the set — the next pass recomputes). That single guard closes
both variants (any intervening commit, local or peer, bumps TxnID).
Amend background-maintenance.md to state the guard as the enforcement
of its invariant. Regression: the demonstrated interleaving (seed →
DeleteRange → commit during detection → reclaim → Check must be
clean because the set was discarded).

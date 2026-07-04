# API-consistency and spec/doc drift sweep (2026-07-04 audit low-grade residue)

**Lands:** audit-burndown-2026-07 chunk 22.

**Severity:** [L] each; batched as one sweep chunk. Items marked
*(decide)* carry a spec-amend or design decision (user granted blanket
authority 2026-07-04).

**Source:** 2026-07-04 full-codebase audit (all five auditors).

## Items

1. `IndexHandle.Range` doesn't validate tuple arity while
   Lookup/LookupKeys/Prefix do (`index.go:745-805`) — add the
   ErrInvalidOptions check.
2. `lock-ordering.md` names nonexistent `db.keyspaceRegistry.mu` and
   omits real `db.mu` / `db.batch.mu` — rewrite against the actual
   lock set (no inversion found among real locks).
3. `leak-detection.md` §Close() Ordering contradicts code and
   transactions.md (writerCh close/drain semantics, step order) —
   amend to match code (transactions.md already does).
4. `pager-slab.md` budget clause counts bitmap/meta buffers the
   implementation doesn't slab-allocate — amend the step-0
   description and budget clause.
5. `limits.md` §Maximum Key Size (~(PageSize-40)/2) vs the actual
   gate `overflowRefFitsLeaf` (~ContentEnd-35) *(decide)* — after
   chunk 2 lands, pick: tighten the gate to the spec bound (keeps the
   oversized-separator regime out) or amend the spec to the
   implemented bound with the separator analysis recorded.
6. Transient stale-meta read while Checkpoint pwrites the active slot
   under lock-free readers (`checkpoint.go:135` vs `read_tx.go:281`)
   — anomaly only (falls back to other slot, reclamation-safe);
   document in durability.md §Live visibility as a bounded exception,
   or close as resolved if chunk 9's shape removes it.
7. All-read-only fleets can never reap stale reader slots (every
   clear path needs LOCK_EX; RO coord refuses AcquireWriter,
   `coord.go:294`; maintenance not started for RO handles,
   `db.go:388`) *(decide)* — document the deployment bound in
   cross-process.md, or allow RO handles a narrow reap-only LOCK_EX
   path.
8. `setKeyspaceCellFree` documents plain/overflow set cells as "rare
   but in-spec" (`set_keyspace.go:997-1001`) while `copy.go:503-505`
   and `check.go:941-943` treat them as ErrCorrupted; no write path
   produces them — align on ErrCorrupted (unrepresentable > tolerated)
   and fix the free-callback comment.
9. Truncated comment fragment at `index_open.go:49` — complete it
   (encoder IDs are covered via synthesized column names in the
   schema hash).

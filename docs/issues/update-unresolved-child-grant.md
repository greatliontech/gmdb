# Update cannot recover the cross-process write grant when a child tx is left unresolved — all writers block until GC

**Lands:** audit-burndown-2026-07 chunk 12.

**Severity:** [M] — demonstrated: `db.Update(ctx, func(tx) error {
tx.BeginChild(); return nil })` leaks the grant; every writer in every
process blocks (`context.DeadlineExceeded`) until a GC cycle fires the
leak cleanup. Update's own no-leak contract (`db.go:897-903`) is
broken; a caller holding the parent has no API to resolve a dropped
child (`activeChild` keeps it reachable, so GC can't recover while the
parent is live).

**Source:** 2026-07-04 full-codebase audit (concurrency auditor).

**Governing spec:** `docs/specs/transactions.md` §Write Batching
clause 5 (cascade) — the batch coordinator already cascades
(`nested.go:123`); Update and top-level Rollback do not.

## Problem

`tx.Commit`/`tx.Rollback` return ErrChildActive (`tx.go:309, 427`)
without releasing the grant; Update's deferred Rollback
(`db.go:906-909`) hits the same freeze.

## Fix direction

Cascade-resolve open descendants in Update's deferred cleanup (and in
top-level Tx.Rollback), as runBatch does via cascadeRollback. Commit
keeps returning ErrChildActive (explicit commit of an unresolved
parent stays an error); Rollback becomes cascading. Amend
transactions.md if its Rollback contract needs the cascade stated.
Regression: the Update reproducer above + a follow-up Begin with a
short deadline succeeding.

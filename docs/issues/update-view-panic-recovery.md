# DB.Update / DB.View leak the write grant / reader slot on a panic in fn

**Lands:** proactive — reachable in-spec resource leak causing unbounded
cross-process writer starvation.

**Severity:** [M]

**Source:** 2026-05-30 deep audit (run `wf_4ad12a2f-039`), raw finding 13.

**Governing spec:** `docs/specs/api-surface.md` (Update/View contract);
`Batch`'s documented recover sets the developer-facing expectation.

## Problem

If `fn` panics inside `Update` (`db.go:755-767`), the panic unwinds past
the `tx.Rollback()`/`tx.Commit()` lines, so the `*Tx` is **never closed**:
the cross-process write grant stays held and pager tx state is never
`AbortTx`'d. Release happens only later via GC leak-detection cleanup
(`tx.go:208` `txCleanupFn`, which requires the `*Tx` to become
unreachable, then releases with a warning). In the interim **every other
writer in this process and in any other process blocks on
`AcquireWriter`** — unbounded writer starvation from a recovered panic
(idiomatic in Go services: an HTTP handler recovering at the top of the
stack and continuing).

`View` (`read_tx.go:330-345`) leaks a reader-table slot the same way:
released only at GC, and while held it pins RPL reclamation (blocking the
writer's free-space reclamation) and consumes a slot toward
`ErrReadersFull`. The developer expectation, set by `Batch`'s documented
recover and the bbolt `Update`/`View` convention this mirrors, is that
these wrappers are panic-safe.

## Fix

Either guarantee cleanup with `defer` so a panic still releases the
grant/slot before propagating:

```go
// Update
committed := false
defer func() { if !committed { tx.Rollback() } }()
// View
defer rtx.Rollback()
```

…**or** explicitly document in `api-surface.md` that `Update`/`View` do
**not** recover panics and that a panicking `fn` leaks the transaction
until GC (making the asymmetry with `Batch` a stated contract, not a
surprise). The deferred-close form also hardens the wrappers against any
future early-return path.

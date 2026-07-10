# Batch closure runtime.Goexit kills the coordinator: permanent deadlock

Lands: 13

## Findings

**[H] A closure exiting via `runtime.Goexit` unwinds the batch
coordinator; the batch tx is neither committed nor rolled back and
every accepted sibling blocks forever.** `batch.go:248-255`
(`invokeClosure` recovers panics only — during Goexit the deferred
`recover()` returns nil), `batch.go:126-137` (coordinator's only defer
is `close(done)`), `batch.go:81` (post-acceptance wait is a bare
`<-call.result` with no ctx select). Consequences: (a) accepted
siblings block forever, even with expired deadlines; (b) the write tx
stays open — the cross-process write grant is held until the GC-based
leak cleanup fires, if ever; (c) `db.batch.started` stays true with no
receiver, so all future no-deadline Batch calls block forever.
Reachable via any closure calling `t.FailNow`/`runtime.Goexit`.
Violates transactions.md §Write Batching ("one misbehaving closure
cannot freeze the batch").

**[L] `cascadeRollback` never re-prices the commit-flush reserve.**
`nested.go:130-144` (vs `rollbackChild`, which calls
`parent.recalcFlushReserve()`); the pager savepoint does not capture
`externalReserve`. After a cascade (batch closure leaves a grandchild
open), the dead child's obligations stay priced into the reserve →
sibling closures can receive spurious ErrTxTooLarge near budget.
Conservative direction only.

**[L] A closure that self-commits its child and returns nil gets
ErrTxClosed back while its write still lands** (`batch.go:49-51`): the
self-commit merged into the parent, which then commits. "Caller
receives an error but the write persists" is stated nowhere and inverts
the usual error contract. Doc-level fix.

## Fix direction

Run closures under a completion flag (set on normal return and in the
panic recovery); a defer in the coordinator/runBatch path detects
Goexit (flag unset, no panic), fails the calling closure, rolls back
the batch tx, and replies to siblings — converting Goexit into the
same isolation as a panic. Decide and pin whether the post-acceptance
wait selects on the caller's ctx. Spec-amend rider: transactions.md
§Write Batching is silent on Goexit and on the post-acceptance ctx
question (surfaced in the audit spec-amend list). Re-price the reserve
in cascadeRollback; document the self-commit outcome.

## Provenance

2026-07-10 defect audit; transaction-layer reviewer. batch_test.go
covers panic/error/unresolved-child, never Goexit; no test asserts the
post-acceptance wait respects ctx.

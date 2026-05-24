# LaggingReader callback

**Lands:** chunk 5.5 — folded by the chunk-5.1 triage gate.
`Keyspace.Put/Delete` and `DeleteRange` are the first call sites
that drive `pager.AllocPage` from B+tree-shaped workloads, which is
the natural exerciser of the "bitmap exhausted → RPL reclamation
blocked → LaggingReader" path. Building the callback in chunk 3
means stand-alone test scaffolding (synthetic readers, synthetic
allocation pressure) with no production caller exercising the same
code path — i.e., dead-on-arrival code by chunk-4-5 time unless the
spec's callback semantics survive untouched, which itself is
unverified without a real caller.

## Summary

Per `lock-ordering.md §Lagging Reader Handling` and
`free-space.md §Page Allocation Priority`, `Options.LaggingReader` is
a callback invoked at most once per `pageAlloc()` call when:

1. The bitmap has no suitable free pages.
2. The RPL has no more reclaimable entries (oldest-reader scan blocks
   advancing the reclamation bound).
3. A reader in the reader table is blocking reclamation.

The callback receives a `LaggingReaderInfo{PID, TxnID, Lag,
HeldPages}` and returns:

- `LaggingReaderWait`: `pageAlloc()` refreshes the reader table and
  retries.
- `LaggingReaderAbort`: `pageAlloc()` returns `ErrDBFull`.

Per `lock-ordering.md` invariant: the callback is invoked at most
once per `pageAlloc()` call to avoid busy loops.

## Why deferred

Chunk 3.4 wired the RPL reclamation bound consumer (writer uses
`coord.OldestReaderTxnID()` at write-tx Begin). The bound-blocking
case is reachable today but the chunk-3 surface has no end-to-end
caller that allocates pages in pressure-driven shapes — `tx.AllocPage`
is exercised by tests only, never by spec-shaped workloads. Without a
real consumer the callback's three-condition gating (bitmap exhausted
AND RPL exhausted AND a reader is the blocking factor) is hard to
exercise except via mocks that may not match the actual integration.

Chunk 5's `Keyspace.Put` is the first real consumer; building the
callback alongside its first call site means:

- The synthetic-test scaffolding is replaced by an integration test
  pinning the chunk-5 + LaggingReader composition.
- The callback's exact firing point in `pager.AllocPage` is reviewed
  alongside the new alloc-pressure shapes the B+tree introduces.
- `LaggingReaderInfo.HeldPages` (estimated pages held unreclaimable
  by the blocking reader) requires a reader-table scan that counts
  RPL entries by TxnID; the count is much easier to define and test
  with chunk-5's real RPL pressure than with isolated synthetic
  retires.

## Spec invariant currently enforced

The `lock-ordering.md §Lagging Reader Handling` invariant is
**recorded but not enforced** (no code, no test) — it becomes a
spec-tier invariant whose `Lands:` resolves to the chunk that first
writes code able to violate it (chunk 5 wires `Keyspace.Put` which
calls `pager.AllocPage` in the spec-shaped way the callback is
designed for). The chunk-5 plan should re-fire the chunk-start gate
on this entry and either fold the work or re-defer with a recorded
reason.

## Out-of-scope for chunk 3

- Options.LaggingReader plumb-through to pager.
- pager.AllocPage's bitmap-exhausted-then-RPL-blocked detection.
- LaggingReaderInfo construction (PID/TxnID lookup from the reader
  table, HeldPages estimation by walking the RPL chain).
- Tests asserting "at most once per pageAlloc" + Wait/Abort
  semantics.

## Cross-references

- `docs/specs/lock-ordering.md §Lagging Reader Handling`
- `docs/specs/free-space.md §Page Allocation Priority` step 4
- `docs/specs/api-surface.md §Types and Options` (LaggingReaderInfo,
  LaggingReaderAction)
- `docs/specs/transactions.md §Lagging-Reader Contract`

# Child-transaction and read-path contract gaps

Lands: 14

## Findings

**[M] `SetFileFormat` on a child transaction is silently dropped at
child commit.** `nested.go:36-71` (BeginChild doesn't seed
`pendingFileFormat`), `nested.go:85-117` (commitChild doesn't merge it),
`tx.go:376` (only the top-level field reaches `pager.Commit`).
`child.SetFileFormat(f)` returns nil (contract: "persisted atomically
with this write transaction", file_format.go:63), child and parent
commits return nil, and the meta's MinSize/GrowStep/ShrinkThreshold are
unchanged — silent loss of an acknowledged mutation; diverges from
transactions.md §Nested Transactions ("child's entries merge into the
parent").

**[L] `View`'s godoc contradicts the code.** `read_tx.go:446-478`: doc
says fn's error is joined with the cleanup error; code returns `fnErr`
alone, discarding a non-nil rollback error (slot-release/munmap
failure).

**[L] Iteration over a frozen or closed-tx keyspace yields a silently
empty sequence.** `iterators.go` (all six closures):
`cursorGuard.require` returns false for
ErrChildActive/ErrTxClosed/ErrClosed, so `for range ks.All()` on a
parent handle while a child is active observes "no data" —
api-surface.md sanctions silent-end only for the *stale* case and
requires ErrChildActive for every operation.

## Fix direction

Seed/merge `pendingFileFormat` through BeginChild/commitChild (savepoint
semantics on rollback); join the rollback error in View per its doc;
surface guard errors from the iterator constructors per api-surface.md
(the iterators' documented error channel — pin the mechanism against
the spec before implementing).

## Provenance

2026-07-10 defect audit; transaction-layer reviewer. No test covers
child+SetFileFormat.

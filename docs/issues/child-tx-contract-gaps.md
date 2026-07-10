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

**[L] `SetCursor.Delete` swallows a structural failure during its
successor re-seek as end-of-iteration.** `set_cursor.go:544-586`:
after `ks.DeleteValue` commits (savepoint released — the deletion is
applied and stays applied), a corrupt-page failure during the
successor re-seek returns nil — spurious success — with the
corruption parked in the outer cursor's `Err()` (`ck, _ :=
c.outerCursor.SeekGE(next); if ck == nil { return nil }`). A
checksums-off drain loop over a set silently terminates early — the
transactions.md invariant's own "silent retention" scenario. No root
desync (markSetCursorsStale refreshes roots before the re-seek); the
plain-keyspace Cursor.Delete analogue was fixed by the read-path
validation chunk (surface + roll back per transactions.md
§Cursor.Delete post-delete state) — mirror that contract here,
deciding whether SetCursor.Delete can roll back (its savepoint is
already released) or must document applied-with-error. Filed from
that chunk's change-set review (adjacent — reproduces on base).

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

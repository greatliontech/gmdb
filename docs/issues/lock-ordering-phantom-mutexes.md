# lock-ordering.md mandates pager.mu and bitmap.mu acquisition order, but neither mutex exists

**Lands:** proactive — load-bearing invariant document references
non-existent locks; spec-vs-code drift.

**Severity:** [L]

**Source:** 2026-05-30 deep audit (run `wf_4ad12a2f-039`), raw finding 26.

**Governing spec:** `docs/specs/lock-ordering.md:58-67`.

## Problem

`lock-ordering.md:58-67` lists an acquisition order that includes
`db.pager.mu` and `db.bitmap.mu`, but **neither mutex exists**:
`internal/pager/pager.go` has no `sync.Mutex` field (grep finds none) and
`internal/bitmap/bitmap.go` has no mutex. No runtime deadlock results
(fewer locks cannot create a cycle), so this is not a correctness defect.
But the lock-ordering invariant is precisely the artifact a future
contributor consults before adding a lock — it references two
non-existent locks, cannot be validated against the code, and could
mislead someone adding concurrency (a future per-keyspace writer, a
concurrent bitmap-summary rebuild) into assuming a guarding lock exists.

## Fix

Update `lock-ordering.md` to describe the locks that **actually** exist
(`writerCh`, `flock`, `activeSlotsMu`, `keyspaceRegistry.mu`,
`closeGate`) and note that pager/bitmap access is serialized by the
exclusive write grant rather than a mutex. **Or**, if these mutexes are
planned, mark them explicitly as not-yet-present so the invariant isn't
read as describing current code.

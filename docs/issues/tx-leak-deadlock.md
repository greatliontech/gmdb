# Leaked write Tx deadlocks the DB until Close

**Lands:** chunk 3 (Write transaction lifecycle + reader table, where
`runtime.AddCleanup`-based leak detection per `leak-detection.md`
arrives).

## Problem

`db.Begin(ctx, true)` acquires `db.writeMu`; `Commit` and `Rollback`
release it. If the caller drops the `*Tx` without calling either,
`writeMu` is held indefinitely and the next `Begin(write=true)` blocks
forever. `DB.Close` does not release `writeMu` either, so even a
clean shutdown from a different goroutine cannot recover.

Concrete repro:

```go
tx, _ := db.Begin(ctx, true) // writeMu held
_ = tx                       // tx leaks (escapes via runtime.GC)
db.Begin(ctx, true)          // blocks forever
```

The chunk-1 implementation defers leak detection to chunk 3 per
`docs/plans/v0-implementation.md §Chunk 3`, but the deadlock surface
is wider than a missing-cleanup nuisance — it's a "wedge the entire
database" failure mode. Quality-bar §"Defer only via a tracked
follow-up" demands an issue doc rather than an in-code TODO.

## Acceptance

Chunk 3 installs `runtime.AddCleanup` on `*Tx` per
`leak-detection.md`. The cleanup invokes `Rollback()` if the tx is
still open at GC time, logging the origin stack trace per spec.
Regression test: leak a `*Tx`, force `runtime.GC()` (or use
`runtime/debug.SetGCPercent`), assert the next `Begin` succeeds.

When this issue closes, the rationale moves inline into the chunk-3
`tx.go` cleanup setup; this file is deleted per the no-cite invariant
in `~/.claude/CLAUDE.md §Issue triage`.

## Notes

The plan's chunk-3 listing has "Leak-detection cleanups on Tx and DB"
under chunk 3, while `docs/plans/v0-implementation.md §Chunk 2`
includes `leak-detection.md` in its Primary specs. Clarify the
chunk-2-vs-chunk-3 boundary for leak detection before chunk 2 starts.

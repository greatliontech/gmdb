# BeginRead racing DB.Close can panic/SIGSEGV instead of returning ErrClosed

**Lands:** audit-burndown-2026-07 chunk 11.

**Severity:** [M] — process crash (panic "AcquireReaderSlot on closed
*File", `internal/lock/reader.go:53`, or SIGSEGV on the unmapped mmap)
instead of the graceful ErrClosed the API promises.

**Source:** 2026-07-04 full-codebase audit (concurrency auditor).

**Governing spec:** `docs/specs/transactions.md` (Begin vs Close);
`docs/specs/leak-detection.md` disclaims Close vs *active*
transactions only — a not-yet-begun BeginRead is the case the
ErrClosed grace paths intend to handle.

## Problem

`read_tx.go:246-309`: after the `closeGate.IsClosed()` check and the
`db.mu` snapshot of `coord`, a concurrent Close can run to completion
— `closeGate.BeginClose` drains only GC cleanups; `txInflight` never
covers in-flight BeginRead — including `lockFile.Close()`
(`db.go:552-554`), before `AcquireReader` executes its slot CAS. The
write path is protected (channel-based AcquireWriter + post-grant
re-checks); the reader path is not.

## Fix direction

Extend the close gate to cover the BeginRead window (inflight counter
or gate re-check after slot acquisition with a release-on-closed
path), mirroring the writer path's post-grant re-check. Regression:
loop `db.View` in one goroutine, `db.Close()` in another, under
`-race`; assert only ErrClosed surfaces.

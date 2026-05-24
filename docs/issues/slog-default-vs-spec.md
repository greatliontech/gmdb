# `slog.Default()` vs spec's `Options.Logger` / `DB.Logger`

**Lands:** when DB gains an `Options.Logger` field. Re-evaluated
at chunk-5.1 — chunk 5 wires the Keyspace API on existing DB
infrastructure without surfacing a new `Options.Logger` field,
so this stays redeferred with `Lands:` unchanged. Note for the
chunk-5.5 chunk-start gate: 5.5 introduces `Options.LaggingReader`
— a user-tunable callback that *uses* logging but does not add an
`Options.Logger` field. If 5.5 ends up needing a logger reference
for the callback's cleanup path, fold this issue then; otherwise
the redefer holds.

## Problem

`docs/specs/leak-detection.md` §Transaction Leak Detection
documents the cleanup as logging via "the `*slog.Logger` on the
`DB` struct." The implementation (chunk 1 + chunk 2.8) uses
`slog.Default()` instead — DB has no Logger field, and Options
has no Logger field.

`docs/specs/api-surface.md` does declare `Options.Logger
*slog.Logger` in the Options block, but the field is not yet
plumbed through Open → DB → cleanups.

The current behavior (slog.Default()) is functionally fine — it
honors the process-wide slog handler — but a future user who
wants per-DB structured logging cannot achieve that via Options
until this is wired.

## Acceptance

1. Add `Logger *slog.Logger` field to `DB` struct (chunk 1 / 2.8
   code in `db.go`).
2. Initialize from `Options.Logger`; fall back to `slog.Default()`
   when nil.
3. Update `txCleanupInfo` and `dbCleanupInfo` to capture the
   logger by pointer (or a `*slog.Logger` directly — slog
   loggers are heap-allocated and survive past `*DB`
   collection).
4. Replace `slog.Default().Warn(...)` calls in `tx.go` /
   `db.go` cleanup functions with the captured logger.
5. Update `leak-detection.md` body if needed (the spec already
   references `*slog.Logger` on DB, so no spec amend is required
   once the code matches).

## Notes

- Pre-existing from chunk 1; surfaced as a Round-2 reviewer nit
  during chunk 2.8 review. Not introduced by chunk 2.8 — the
  chunk-2.8 code uses `slog.Default()` matching the chunk-1
  pattern.
- Filing rather than blocking 2.8 because the spec's Options
  block already declares the field; the wire-up belongs in the
  chunk that first surfaces a user-facing logger need.

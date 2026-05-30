# `Open` takes a `context.Context` it ignores

**Lands:** condition — before the first tagged release (signature
change is breaking).

## Problem

`db.go:133` — `func Open(_ context.Context, path string, opts Options)
(*DB, error)`. The context parameter is named `_` and is never
referenced in the body. The signature implies a cancelable open, but
the create / mmap / `fdatasync` syscalls it performs never observe the
context.

This also diverges from `docs/specs/api-surface.md §Open`, which
documents `func Open(path string, opts *Options) (*DB, error)` — no
context, `*Options` by pointer. Implementation and spec disagree on the
constructor signature (the `opts Options`-by-value choice is fine and
arguably better than the spec's pointer; the ctx is the real
discrepancy).

## Resolution

Either:

1. **Honor it** — thread the context into the bounded I/O (a deadline
   on open/init, checked at `readPersistedPageSize`'s retry loop, which
   already sleeps), so `Open(ctx, …)` is genuinely cancelable; or
2. **Drop it** — `Open(path string, opts Options)`. Pre-v1 with no
   installed base, the clean break is dropping the unused parameter.

Reconcile the spec to whichever is chosen.

## Notes

Surfaced during the 2026-05-30 architecture/factoring audit (public API
surface pass).

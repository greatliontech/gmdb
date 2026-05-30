# `Begin(ctx, write bool)` — the bool parameter is vestigial

**Lands:** condition — before the first tagged release (signature
change is breaking).

## Problem

`db.go:521` — `func (db *DB) Begin(ctx context.Context, write bool)
(*Tx, error)`. The `write` argument must always be `true`: the body
opens with `if !write { return nil, ErrReadOnly }`, and read
transactions go through the separate `BeginRead` (returning `*ReadTx`).
So every correct call site is `Begin(ctx, true)` and the parameter
encodes no real choice.

The godoc explains it as a deliberate "loud-fail for legacy callers" of
the old unified-`Tx` `Begin(ctx, writable)` surface — but this is a
pre-v1 database (`development: true`) with no installed base. Keeping a
must-always-be-`true` parameter to protect callers that do not exist is
backcompat scaffolding for a non-existent base, which the project's
own clean-break convention classes as over-engineering.

## Resolution

Collapse to `Begin(ctx context.Context) (*Tx, error)`. Remove the
`ErrReadOnly`-on-`!write` branch (the type system already prevents
write methods on `*ReadTx`). Update `docs/specs/api-surface.md`, which
still documents the `writable bool` form and its chunk-3 history.

## Notes

Surfaced during the 2026-05-30 architecture/factoring audit (public API
surface pass). Same clean-break rationale as
`open-ignores-context.md` — both are constructor-surface cruft kept for
a non-existent installed base.

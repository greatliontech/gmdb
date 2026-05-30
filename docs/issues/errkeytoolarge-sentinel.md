# ErrKeyTooLarge documented as public but not a gmdb sentinel; internal error leaks unwrapped from Put and BulkLoad

**Lands:** proactive — doc-vs-code contradiction; a documented error
cannot be detected through the public surface.

**Severity:** [L]

**Source:** 2026-05-30 deep audit (run `wf_4ad12a2f-039`), raw finding 22;
also surfaced by the completeness pass.

**Governing spec / docs:** `keyspace.go:543` godoc (names the error);
`errors.go:7-264` (no such sentinel).

## Problem

The `keyspace.go:543` godoc documents `ErrKeyTooLarge` as a returned
error, but it is **not a gmdb sentinel** — `errors.go` declares no such
symbol. The internal `btree.ErrKeyTooLarge` leaks **unwrapped** from
`Put` (`mapBtreeErr`, `keyspace.go:1528-1539`) and from `BulkLoad`
(`bulkLeafEntry`, `bulkload.go:732-741`). A caller doing
`errors.Is(err, gmdb.ErrKeyTooLarge)` cannot compile (no symbol) and
cannot reliably detect the oversize-key condition through the documented
public surface — they'd have to import the internal package (impossible)
or string-match. Same defect shape as `ErrVersionMismatch` had before it
was wired this session.

**Relationship to the split bug:** the count-vs-byte split bug
(`btree-byte-balanced-split`) currently *surfaces* as this leaked error on
valid data. This sentinel fix is still independently required: even after
the split fix, a genuinely oversize key must return a wrapped, detectable
`gmdb.ErrKeyTooLarge`.

## Fix

Declare `var ErrKeyTooLarge = errors.New(...)` in `errors.go` and
translate `btree.ErrKeyTooLarge` to it inside `mapBtreeErr` (covering
`Put`/`Delete`/`Get` and, via `bulkLeafEntry`'s caller, `BulkLoad`).
**Or** remove the `ErrKeyTooLarge` mention from the `Put` godoc and
document the actual returned error until the sentinel is added. Add a
test asserting `errors.Is(err, gmdb.ErrKeyTooLarge)` on a genuinely
oversize key via both `Put` and `BulkLoad`.

# `Cursor.Err()` and `SetCursor.Err()` omit ErrCursorUnpositioned for the Unpositioned state

**Lands:** opportunistic — when the cursor state-machine spec
`transactions.md §Cursor State Machine` is next audited, OR when a
caller explicitly relies on `Err() == nil ⟺ positioned`.

## Problem

`transactions.md §Cursor State Machine` row 1 states that the
Unpositioned state's `Err()` returns `ErrCursorUnpositioned`. The
chunk-5 `Cursor.Err()` (`keyspace.go:959`) and the chunk-6.7
`SetCursor.Err()` (`set_cursor.go:547-567`) both omit this:
they check closeErr → dead → stale → outerCursor.Err but never
the `positioned` flag.

Concrete: a freshly-constructed cursor's `Err()` returns `nil`,
indistinguishable from a positioned cursor. A caller using
`if c.Err() != nil` to detect "needs re-position" cannot
distinguish Unpositioned from EOI.

## Acceptance

Either:

1. Amend `transactions.md §Cursor State Machine` to drop the
   Unpositioned-Err-is-ErrCursorUnpositioned requirement (the
   chunk-5/6 implementation suggests `Err() == nil` is the
   "no-error" signal, with `Current() == (nil, nil)` being the
   "not at a value" signal). Then add an explicit clause: "Err()
   returns nil in Unpositioned / EOI / value-EOF / value-BOF;
   callers detect these via `Current() == (nil, nil)`."
2. Implement: add `if !c.positioned { return ErrCursorUnpositioned }`
   to both Cursor.Err and SetCursor.Err.

The current behavior matches option (1) implicitly but the spec
matches option (2). User decides.

## Notes

Surfaced by chunk-6.7 Round-1 adversarial review (M-2). Adjacent
to the chunk-5 Cursor.Err omission, hence the shared issue doc.
Pre-existing behavior; not a regression.

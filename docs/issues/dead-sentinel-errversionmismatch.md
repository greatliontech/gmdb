# `ErrVersionMismatch` is a declared-but-never-returned sentinel

**Lands:** condition — decide before the first tagged release. If
removed, that is a breaking change to the exported error set and must
land while `development: true`; if kept, it needs a real `Lands:`
trigger for when it starts being returned.

## Problem

`errors.go:55` declares `ErrVersionMismatch`, but `git grep` confirms
it is **never returned** anywhere outside its own declaration — only
`errors.go:52` (godoc) and `:55` (the `var`) reference it. Its godoc
admits it: "Reserved for future format evolutions; never returned in
v0."

An exported sentinel a caller can `errors.Is` against but that the
engine never produces is dead public surface — it invites a branch that
can never fire.

## Resolution

Pick one:

1. **Remove it** (clean break, pre-v1) — re-add when format-version
   checking actually lands and has a return site.
2. **Keep it** only if a concrete near-term consumer is planned — and
   then convert this issue's `Lands:` to "when on-disk format-version
   checking is implemented" and ensure the implementing change wires a
   return site in the same change set (a sentinel and its first
   producer should land together).

The rest of the 44-sentinel set was spot-checked and confirmed live
(`ErrCoveringTupleMalformed`, `ErrBatchClosurePanic`,
`ErrIndexEncoderIDEmpty`, … all have non-test return sites);
`ErrVersionMismatch` is the only dead one.

## Notes

Surfaced during the 2026-05-30 architecture/factoring audit (public API
surface pass).

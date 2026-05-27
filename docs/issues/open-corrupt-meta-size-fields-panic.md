# Open panics (instead of `ErrCorrupted`) on corrupt meta size/offset fields

**Lands:** opportunistic — Open-time corrupt-meta robustness hardening
(sibling to the RPL out-of-range guard in `rebuildRPLChain`), or when a
meta-fuzzing pass is added alongside `TestCorruptionSentinelOnOpen`.

## Problem

`page.ValidateMeta` (`internal/page/meta.go`) checks only `Magic`,
`Version`, `PageSize`, and `Flags`. It validates **no size/offset
field** — `BitmapPages`, `MaxSize`, `HighWaterMark`, `KeyspaceRoot`,
`NumKeyspaces` — against each other or against the actual file. A
checksum-verifying meta (an attacker or a torn write that happens to
re-hash consistently, or a deliberately forged file) can therefore set
these fields arbitrarily and make `Open` **panic** rather than return a
graceful `ErrCorrupted`.

Demonstrated instance (`internal/pager/init.go:238-240`, the in-memory
bitmap rebuild):

```go
bitmapBytes := uint64(m.BitmapPages) * uint64(pageSize)
detail := make([]byte, bitmapBytes)
copy(detail, p.mmap[2*uint64(pageSize):2*uint64(pageSize)+bitmapBytes])
```

A corrupt `BitmapPages` (e.g. `1<<30`) makes `bitmapBytes` exceed the
mmap reservation (`MaxSize * pageSize`), so the
`p.mmap[... : 2*pageSize+bitmapBytes]` slice expression panics with a
slice-bounds-out-of-range (or the `make` OOMs first). Surfaced and
reproduced by the chunk-12 RPL-panic fix's Round-1 adversarial review.

`MaxSize` itself is also unvalidated and drives the mmap reservation at
`init.go:229` (`reservation = m.MaxSize * pageSize`); a forged-huge
`MaxSize` requests a huge reservation. (`KeyspaceRoot` /
`NumKeyspaces`-driven B+tree walks are already `hwm`-bounded via
`btree.WalkKV`, so those are not in this gap.)

## Class

`class=adjacent` per the chunk-12 RPL-fix diff arbiter — the cause-lines
(`init.go:238-240`, `init.go:229`) are outside the RPL-walk change set
and reproduce on its base (HEAD before the fix). Same **fault class** as
the resolved RPL out-of-range panic (Open must be total over arbitrary
on-disk meta bytes: corruption ⇒ `ErrCorrupted`, never a crash), but a
distinct proximate line and meta field.

## Fix sketch

Two shapes (maintainer decides; the established codebase pattern is the
walk-site clamp, e.g. `checker` clamps `hwm = min(fileSize/PageSize,
MaxSize)` and emits `HighWaterMarkOutOfRange` rather than rejecting in
`ValidateMeta`):

1. **Walk/use-site bounds.** Bound `bitmapBytes` (and any other
   size-field-derived allocation/slice) against the mmap reservation /
   file extent before use, returning `ErrCorrupted` on overflow —
   mirroring the RPL `backedPages` guard and `Pager.Page`'s Inv-RV3.
2. **`ValidateMeta` extension.** Add cross-field invariants
   (`HighWaterMark <= MaxSize`, `BitmapPages == ceil(MaxSize / (PageSize
   * 8))`, reservation fits addressable space) so every consumer
   inherits the guarantee. Note `check.go:349-355` documents a
   deliberate reason the current design clamps at walk time rather than
   rejecting in `ValidateMeta` (avoid OOM/SIGBUS without rejecting
   recoverable databases) — reconcile with that stance.

A meta-fuzzing test (flip each size/offset field, re-hash, assert
`Open` returns `ErrCorrupted` and never panics) is the natural
regression home.

## Notes

The resolved RPL issue closed the `rebuildRPLChain` instance of this
class (out-of-range RPL segment pointers); this issue tracks the
remaining Open-time size/offset fields. When it resolves, the
load-bearing rationale moves inline into the bounded use-sites (or the
`ValidateMeta` invariants) and this file is deleted per the no-cite
invariant.

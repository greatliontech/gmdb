# Index registry decoder lacks pre-check on count×2 vs remaining data

**Lands:** chunk 11 (`Check()` integrity walk — the natural home
for on-disk corruption tolerance: padding-zero strictness, registry-
tree bounds, branch-page validation per
`btree-branch-page-validation.md`, etc.)

## Problem

The chunk-7.3 index registry decoder
(`index_codec.go:decodeRegistryEntry`) bounds-checks every
length-prefixed field on a per-iteration basis but does NOT
pre-check `colCount * 2` (or `covCount * 2`) against
`len(data) - off` before allocating the `make([]string, colCount)`
slice. A maliciously-large `ColumnCount = 65535` on a truncated
on-disk entry forces allocation of a ~1.5 MB string slice before
the per-iteration bounds check trips on the first NameLen read.

Same shape applies to `registryList` (`index_codec.go:registryList`):
a corrupted-on-disk registry tree handing out unbounded numbers of
keys / per-key lengths can allocate unbounded memory before the
btree cursor's bounds check fires.

This is purely an **adversarial input / on-disk corruption tolerance**
issue. The decoder is internal-only at chunk 7 (no public surface
exposes raw registry bytes). A malicious caller cannot reach the
decoder without already having write access to the registry sub-tree.
The realistic threat model is on-disk corruption from external
causes (filesystem bit-rot, ill-behaved tools, intentional tampering)
— precisely the threat model `Check()` is designed to surface.

## Acceptance

Two cheap pre-checks at chunk 11 hardening time:

1. `decodeRegistryEntry`: before each `make([]string, count)`, verify
   `count * 2 + off <= len(data)` (each entry has at least a 2-byte
   NameLen header). Reject early with a wrapped `errRegistryEntryShort`
   if not.

2. `registryList`: bound the returned `names` slice's total byte
   count against a sensible cap (e.g. the engine-configured PageSize
   or an explicit registry-namespace cap). Surface
   `ErrCorrupted`-wrapped error past the cap.

Both checks land naturally with `Check(CheckIndexes)` from chunk 11
which walks every registry tree end-to-end with strict bounds.

When this issue closes, the load-bearing rationale moves inline
into the relevant `Check()` invariant or hardening docs and this
file is deleted per the no-cite invariant.

## Notes

Surfaced by the chunk-7.3 Round-1 adversarial review (M-1 + L-2
findings); promoted from inline forward-reference comments to a
tracked issue doc at chunk-7.3 Round 2 per the workflow's "Defer
only via a tracked follow-up — an issue doc" rule. The original
inline deferral comment in `registryList` was trimmed at this
filing.

Adjacent issues with the same chunk-11 trigger:
`btree-branch-page-validation.md` (defense-in-depth branch-page
validation across `btree.Get`/`Has`/`Delete`/`Cursor`/`FreeSubtree`).

# Compressed-leaf SharedLen is trusted at decode; forged/bit-flipped page passes Validate then panics or fabricates keys

**Lands:** audit-burndown-2026-07 chunk 1.

**Severity:** [M] — with `PageChecksum=false` (supported config), a
single-byte flip in a delta entry's SharedLen field either panics
(slice out of range) or silently reconstructs wrong keys.

**Source:** 2026-07-04 full-codebase audit (btree/pager auditor).

**Governing spec:** `docs/specs/page-formats.md` (compressed leaf);
decoder-robustness contract as documented on `ValidateBranch`.

## Problem

`Validate` deliberately skips the SharedLen-vs-previous-key semantic
check (`internal/page/leaf.go:557-560`), but `decodeDeltaEntry` slices
`prevKey[:sharedLen]` unguarded (`internal/page/leaf_compressed.go:96,
109, 122`). For a restart-key-backed `prevKey` (cap = rest of page),
`sharedLen ∈ (keyLen, cap]` silently prepends adjacent page bytes to
the reconstructed key — wrong keys, wrong search results, undetected;
`sharedLen > cap` panics. Violates the package's never-panic-on-forged-
page contract. Reachable via bitrot whenever checksums are off.

## Fix direction

Bound `SharedLen ≤ len(prevKey)` in `validateDeltaEntry` (track the
decoded key length through the Validate walk). Regression: 2-entry
compressed leaf, overwrite SharedLen with 0xFFFF, assert Validate
rejects (today: Validate == nil, then SearchLeaf/Iter panics).

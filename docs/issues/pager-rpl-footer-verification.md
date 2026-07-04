# RPL segment pages are never checksum-verified on the reclamation and Open chain-walk paths

**Lands:** audit-burndown-2026-07 chunk 4.

**Severity:** [H] — a decodable bit-flip in a committed RPL segment
either panics (`Bitmap.checkAllocatable`,
`internal/bitmap/bitmap.go:500-507` — crash on corrupt input, against
integrity.md's error-not-crash contract) or frees a live tree page →
silent corruption after reuse.

**Source:** 2026-07-04 full-codebase audit (durability auditor).

**Governing spec:** `docs/specs/checksums.md` — footer covers every
data page "(Branch, Leaf, Overflow, RPL segment)"; "every page read
from the pager is verified on first access".

## Problem

`reclaimRPL` (`internal/pager/freespace.go:449-451`) and
`rebuildRPLChain` (`internal/pager/init.go:722-723`) read segments via
`pageRaw`, which skips footer verification
(`internal/pager/pager.go:670-701`); `DecodeRPLSegment` checks only
page type + count (`internal/page/rpl.go:66-85`). The `DecodeRPLSegment`
godoc ("the pager performs footer verification before calling") and the
`reclaimRPL` step-1 comment ("immutable and footer-verified at read
time") are false at every production call site. The quarantine fix
(31ea454) only catches structural decode failure, so a bit flip in the
PageIDs array or Count that leaves Type intact passes decode and feeds
wrong ids to `bitmap.Set`.

## Fix direction

Verify the footer (when `PageChecksum` is on) before decoding in both
walkers; on mismatch take the existing quarantine path (reclaim) /
`ErrCorrupted` (Open head) or reclaimed-tail stop (Open non-head).
Fix the two false comments. Consider hardening the bitmap Set call
sites to return ErrCorrupted on out-of-range ids instead of panicking.
Regression: churn to produce an on-disk segment, flip one entry byte,
reopen, force reclamation; assert quarantine/error, not panic, and
that no live page is freed.

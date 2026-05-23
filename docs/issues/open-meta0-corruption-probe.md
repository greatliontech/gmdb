# `Open` cannot recover from a torn meta-0 `PageSize` field

**Lands:** chunk 11 (Check + integrity).

## Problem

`internal/pager/init.go` `Open` reads the first 144 bytes of the file
to decode meta 0, then uses meta 0's `PageSize` field to locate meta 1
at offset `PageSize`. If meta 0's `PageSize` is the corrupted field
(single-byte flip in offset 8-11), `Open` rejects with "meta0
PageSize invalid" even when meta 1 is fully intact.

The dual-meta atomicity invariant (file-layout.md, kind=entailed)
guarantees recovery via fallback to the still-valid meta in this
exact scenario — but only if the recovery path can find meta 1.

## Options

1. **Probe at Open.** Iterate `ValidPageSize` candidates (4 KB,
   8 KB, 16 KB, 32 KB, 64 KB); for each candidate PS, read
   MetaPayloadSize bytes at offset PS, run `VerifyMeta`; if it
   verifies and `m.PageSize == candidate`, use it as active. Five
   pread syscalls in the worst case; cost only on the rare torn-
   meta-0 path.
2. **Move PageSize into a header sidecar.** Store PageSize in a
   tiny anchor at offset 0 (4 bytes, with a CRC) that is independent
   of either meta page. Bigger spec change; out of scope for chunk
   11.
3. **Accept the limitation.** Document that recovery requires a
   recoverable meta-0 PageSize field. Chunk-11 `Check()` repair path
   could rewrite meta 0 from meta 1's content.

Option 1 is the smallest correct change and lives in chunk 11 as part
of the integrity-check repair surface.

## Acceptance

`Open` on a database where meta 0's PageSize bytes are zeroed (but
the rest of meta 0 may also be corrupt) succeeds via meta-1 fallback,
returning a working `*Pager` whose subsequent `Commit` rewrites both
metas to a consistent state.

Regression test: zero bytes 8-11 of meta 0, ensure Open recovers via
meta 1.

When this issue closes, the rationale moves inline into the chunk-11
`Check()` / repair code path; this file is deleted per the no-cite
invariant in `~/.claude/CLAUDE.md §Issue triage`.

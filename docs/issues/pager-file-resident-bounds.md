# Pager file-resident bounds: stale fileSize, shrink SIGBUS window, MaxSize clamp

Lands: 8

## Findings

**[M] Writer's stale `fileSize` after a crash-recovered
bit-set-above-truncated-EOF state yields spurious ErrCorrupted on
healthy committed pages.** `internal/pager/freespace.go:171-189`
(bitmap-path alloc never calls `ensureFileCovers`),
`internal/pager/pager.go:985-989` (`Page` bounds by `p.fileSize`),
`internal/pager/commit.go:389-399` (step-1 pwrite past EOF extends the
file without updating `p.fileSize`). Post-crash recovery adopting epoch
D can present a free page P below HWM_D but at/above fileSize (lazy
tail-refund bit-clears unflushed + ftruncate metadata journaled);
`AllocPage` returns P, commit pwrites (extends the file silently), and
every subsequent writer-side `Page(P)` fails the file-resident bound →
ErrCorrupted on a legitimately committed page until reopen.

**[M] Concurrent shrink invalidates a live read-tx's file-resident
bound: SIGBUS instead of ErrCorrupted on corrupt input.**
`internal/pager/commit.go:501-521` (`maybeShrink` ftruncates
immediately post-commit), `pager.go:343-371` (reader `fileSize` fixed
at BeginRead), `pager.go:985-994`. A reader opened at size X after the
writer truncates to Z<X still admits ids in [Z,X); an mmap access
there (reachable only via a content-derived forged/bitrot page id —
exactly the input class checksums.md §Structural and Allocation Bounds
promises yields ErrCorrupted, "never a SIGBUS") is process-fatal.

**[L] `Page()` file-resident bound not clamped to MaxSize.**
`pager.go:985` computes `backedPages = fileSize/PageSize` unclamped;
a file externally grown past the mmap reservation plus a corrupt
pointer → slice-bounds panic instead of ErrCorrupted. `attachState`/
`rebuildRPLChain` already use `min(fileSize/PageSize, MaxSize)`.

## Fix direction

Track file extension through the alloc/commit path (update `fileSize`
when step-1 pwrites extend, or route bitmap-path allocs through
`ensureFileCovers`); bound reader page access by a shrink-stable limit
(e.g. clamp to the snapshot meta's HWM, which the reclamation-bound
argument already guarantees is tree-safe) so corrupt ids fail the
bound instead of faulting; clamp `Page()` to MaxSize like the other
sites.

## Provenance

2026-07-10 defect audit; pager/commit reviewer.

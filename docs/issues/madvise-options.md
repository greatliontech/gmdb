# madvise Options (PreloadPages / HugePages / ReclaimOnClose) spec'd but absent

**Lands:** proactive — spec'd Options + mmap-strategy promises with no
field and no `MADV_*` call anywhere.

**Severity:** [M]

**Source:** 2026-05-30 completeness pass (this audit session).

**Governing spec:** `docs/specs/mmap-strategy.md` and `overview.md`
(Design Decisions) describe the madvise-driven preload / huge-page /
reclaim-on-close behaviour.

## Problem

The mmap tuning surface is entirely absent: the `Options` struct has no
`PreloadPages` / `HugePages` / `ReclaimOnClose` fields, and there is **no
`MADV_*` call anywhere** in the implementation, despite `mmap-strategy.md`
and `overview.md` specifying the behaviour. A user cannot request
`MADV_WILLNEED` preload, `MADV_HUGEPAGE`, or `MADV_DONTNEED`-on-close.

## Fix

Add the `Options` fields and wire the corresponding `unix.Madvise` calls
into the mmap setup / teardown path (`MADV_WILLNEED` for `PreloadPages`,
`MADV_HUGEPAGE` for `HugePages`, `MADV_DONTNEED` for `ReclaimOnClose`),
guarded per-platform. **Or**, if deferring, file a concrete deferral and
trim the `mmap-strategy.md` / `overview.md` promises to match the
implemented surface. Cover with a smoke test that the advice calls are
issued (and tolerated where unsupported).

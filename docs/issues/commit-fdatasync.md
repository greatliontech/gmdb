# Commit uses fsync where every spec and the Design Decisions table specify fdatasync

**Lands:** proactive — spec-vs-code divergence on a documented design
choice (correctness-safe; perf rationale unmet).

**Severity:** [L]

**Source:** 2026-05-30 deep audit (run `wf_4ad12a2f-039`), raw finding 24.

**Governing spec:** `docs/specs/durability.md §Durability Modes`;
`overview.md` Design Decisions row "Commit I/O | pwrite … + fdatasync".

## Problem

Commit calls `fsync` (`os.File.Sync`) at `internal/pager/commit.go:123`
(step 2), `:162` (step 4), and `:392` (`maybeShrink`), where every spec
and the Design Decisions table specify **fdatasync**. This is
correctness-safe — `fsync` is strictly stronger (it also flushes inode
metadata) — so there is no data-loss bug. But `fdatasync` was chosen
specifically to avoid the inode-metadata flush on the per-commit hot path;
with `fsync`, every Durable/DataOnly commit pays that extra flush the
design intended to avoid. The Go stdlib does not expose `fdatasync`
portably, which likely forced `fsync` — but then the spec's perf rationale
is silently unmet.

## Fix

Either **(a)** implement `fdatasync` via
`golang.org/x/sys/unix.Fdatasync` on Linux/FreeBSD with an `fsync`
fallback elsewhere, matching the spec; **or (b)** surface to the user that
the stdlib forces `fsync`, amend `durability.md` / `overview.md` to record
`fsync` as the actual mechanism, and drop the fdatasync-specific perf
rationale. Do not leave spec and code diverged silently.

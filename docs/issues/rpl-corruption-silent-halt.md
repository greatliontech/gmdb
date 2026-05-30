# Corrupt RPL segment during allocation halts reclamation silently and grows the file

**Lands:** proactive — diagnostic/policy gap (a deliberate
availability-over-fail-fast choice that is currently undocumented).

**Severity:** [L]

**Source:** 2026-05-30 deep audit (run `wf_4ad12a2f-039`), raw finding 25.

**Governing spec:** `docs/specs/free-space.md §RPL Reclamation` (does not
currently require fail-fast here — so the behaviour is defensible, just
undocumented).

## Problem

A single corrupt/torn RPL segment makes all pages in that segment and
every older segment permanently unreclaimable until a manual `Check()`
runs (`internal/pager/freespace.go:418-452` `reclaimRPL`; `:191-210`
AllocPage RPL tier → file extension). The writer keeps extending the file
past genuinely-free space and at `MaxSize` surfaces a misleading
`ErrDBFull` while the bitmap shows plenty of (RPL-trapped) free pages. The
corruption is invisible to the application until it independently runs
`Check()`. This is a deliberate availability-over-fail-fast choice (the
commit comment is explicit) and is not a spec violation — but the
`ErrDBFull`-vs-`RPL-is-corrupt` divergence is a poor diagnostic and risks
an operator mis-diagnosing capacity instead of corruption.

## Fix

Decide and **document** the policy in `free-space.md §RPL Reclamation`:
either return a distinct wrapped `ErrCorrupted` from `AllocPage` when
`reclaimRPL` aborts on a malformed segment (fail-fast, lets the caller
poison/repair), **or** keep the halt-and-extend behaviour but emit a
`slog` warning naming the bad segment page and set a flag that
`DBStats`/health surfaces, so the corruption is discoverable without an
explicit `Check()`.

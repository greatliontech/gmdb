# Issues

Tracked follow-ups for gmdb. Each entry is a `docs/issues/<slug>.md`
file with a `Lands:` trigger (a chunk number or a condition). Per
the workflow in `~/.claude/CLAUDE.md` (Issue triage), this index was
walked at every chunk-start gate (`N.1`) during the chunk roadmap —
entries whose `Lands:` resolved to the current chunk were folded,
redeferred, or closed.

The v0 chunk roadmap and the architecture-consolidation plan are
complete; their plans were deleted at close-out (`git log --all --
docs/plans/<name>.md` recovers them). The active plan is
`docs/plans/defect-audit-remediation.md`; its chunk-start gates (`N.1`)
walk this index, and entries may also be pulled as a proactive
burn-down — each resolved as its own change set: diagnose → fix →
regression test → adversarial review → promote-then-delete.

When an issue is resolved, the load-bearing rationale moves inline
into the spec / code where it belongs (kept-current artifact), all
cites are repointed at the new home, and the issue file is deleted.
`git log --all -- docs/issues/<file>.md` preserves the history.

This backlog spans **three audit waves**, kept in separate tables below
because their severity scales differ:

1. **Architecture / factoring audit** (concept, package decomposition,
   public API surface, composition/duplication). Its `[H]`/`[M]`/`[L]`
   tags mean High = public-API correctness/leak, Medium =
   factoring/duplication, Low = naming/surface nit.
2. **Completeness / correctness / algorithm audit (2026-05-30)** — run
   with the project treated as **in-progress, not finished**. Its
   `[H]`/`[M]`/`[L]` scale is **correctness-based**: High = a reachable
   in-spec input/state that yields a wrong result (data loss, corruption,
   use-after-reclaim); Medium = a spec'd capability missing or a reachable
   resource leak; Low = doc/spec-vs-code drift, diagnostics, or
   latent/narrow-reachability defects. 27 confirmed findings (2 refuted)
   collapse to the rows below by **shared fix** — the provenance (raw
   finding numbers, audit run `wf_4ad12a2f-039`) is recorded in each issue
   file. Several spec'd-but-unbuilt rows carry an explicit
   *implement-or-document* decision for the user; the cross-process rows
   carry a scope decision (is concurrent multi-process write in v0?).
3. **Defect audit (2026-07-10)** — nine subsystem reviewers, each
   reading its governing spec first; every finding carries a concrete
   reachable failure scenario; three H findings reproducer-confirmed.
   Same correctness-based `[H]`/`[M]`/`[L]` scale as wave 2. Findings
   collapse to rows by **shared fix**; each issue file records its
   constituent findings with file:line cites and any spec-amend riders.
   None of the findings are caught by the existing suite (green under
   `-race` on HEAD at filing time).

In all three, the severity tag is an **audit artifact, not a work plan**.
Several factoring rows are pre-v1 **clean-break** candidates gated by the
first tagged release (`development: true`, `.semrel.yaml`). Resolved
entries are removed from their table and preserved in git history
(`git log --all -- docs/issues/<file>.md`).

## Open — architecture / factoring audit

| Slug | Lands | Summary |
|------|-------|---------|

## Open — completeness / correctness / algorithm audit (2026-05-30)

Filed from deep audit run `wf_4ad12a2f-039` (7 finders + adversarial
verification; 27 confirmed, 2 refuted) plus a completeness pass. Rows are
severity-ordered (H → M → L); the `Subsumes` notes record which raw
findings each shared-fix row covers.

| Slug | Lands | Summary |
|------|-------|---------|

*None open — resolved issues live in git history
(`git log --all -- docs/issues/<slug>.md`).*

## Open — defect audit (2026-07-10)

`Lands:` numbers refer to `docs/plans/defect-audit-remediation.md`
chunks (bottom-up by layer, grouped by function).

| Slug | Lands | Summary |
|------|-------|---------|
| subpage-promotion-single-leaf-cap | 5 | [H] promotion packs all members into one leaf: small-member sets hard-cap (~254 at fvs=8), reproduced |
| fixed-size-nested-leaf-compaction | 6 | [L] spec'd compact fixed-size nested-leaf cells not implemented |
| rpl-half-reclaimed-segment-double-free | 7 | [H] crash image re-includes a half-reclaimed RPL segment → later double-free of a re-allocated live page |
| pager-file-resident-bounds | 8 | [M×2] stale writer fileSize → spurious ErrCorrupted; reader SIGBUS window after shrink; [L] Page() MaxSize clamp |
| pager-commit-residue | 9 | [L×3] checkpoint under-anchor; armed rplRelocFloor survives abort; relocation probe undercounts segments |
| reader-slot-clear-validation | 10 | [H] stale-reader clear without occupancy re-check evicts a live reader (use-after-reclaim); [M] frozen-reader ghost-store |
| lockfile-stale-removal-race | 11 | [H] unguarded unlink-by-name on stale lock file → split brain, two writers |
| lock-boot-epoch | 12 | [H] post-reboot future heartbeats + starttime collisions bypass the recovery gate; [L] lock file never deleted contra spec |
| batch-goexit-deadlock | 13 | [H] closure Goexit kills coordinator, permanent deadlock; [L] cascade reserve re-price; [L] self-commit outcome doc |
| child-tx-contract-gaps | 14 | [M] child SetFileFormat silently dropped; [L] View error join; [L] iterators silently empty on guard errors; [L] SetCursor.Delete swallows re-seek corruption as end-of-iteration |
| db-leak-detection-pinning | 15 | [M] daemon goroutines pin *DB — handle-leak detection unreachable; [L] LaggingReader reentrancy deadlock undocumented |
| nested-keyspace-handle-resurrection | 16 | [M] child delete+recreate resurrects parent's dead handle (both kinds), reproduced |
| index-ondelete-partial-state | 17 | [M] extractor panic mid-onDelete commits partial index state; [L] stale coverValue → false ErrCorrupted; [L] schemaHash doc grammar |
| bulkload-index-parity | 18 | [H] index bulk build skips key-size gate → un-updatable/un-compactable DB; [M] no overflow promotion for index values; [L] config parity |
| copyto-hardening | 19 | [M×2] verbatim CopyTo SIGBUS on truncated source; torn destination on crash; [L] Check misses overflow-header corruption |
| maintenance-reclaim-truncated-walk | 20 | [H] leak reclamation behind a truncated RPL walk double-frees pages still in the live chain |
| compaction-consolidating-alloc | 21 | [M] relocations re-land in the evacuation band — no convergence; spec's consolidating allocator unimplemented |
| spec-descriptive-drift | 22 | [L] batch: spec clauses describing mechanisms the code doesn't use; code-shape content in specs |

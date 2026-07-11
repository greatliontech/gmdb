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
| checkpoint-selfdurable-anchor-persist | 23 | [L] pure-SyncDataOnly Checkpoint leaves the persisted anchor trailing by one (peer reclamation delayed); persisting would rewrite the assertion's sole durable carrier in place — tear hazard |
| copyto-hardlink-destination-support | when a non-hard-link destination filesystem is decided to be a supported CopyTo target | [L] publish fails on vfat/exfat/FUSE targets; [nit] NFS link() retransmission quirk |
| reclaimed-boundary-torn-peer | when grant-handoff tear detection or reclaimed-boundary gating is settled | [H] surviving handle's chain predates a peer's torn never-published reclamation; reclamation behind the reclaimed boundary double-frees |
| compaction-consolidating-alloc | 21 | [M] relocations re-land in the evacuation band — no convergence; spec's consolidating allocator unimplemented |
| spec-descriptive-drift | 22 | [L] batch: spec clauses describing mechanisms the code doesn't use; code-shape content in specs |

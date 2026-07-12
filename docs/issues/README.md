# Issues

Tracked follow-ups for gmdb. Each entry is a `docs/issues/<slug>.md`
file with a `Lands:` trigger (a chunk number or a condition). Per
the workflow in `~/.claude/CLAUDE.md` (Issue triage), this index was
walked at every chunk-start gate (`N.1`) during the chunk roadmap —
entries whose `Lands:` resolved to the current chunk were folded,
redeferred, or closed.

The v0 chunk roadmap, the architecture-consolidation plan, and the
defect-audit remediation plan are complete; their plans were deleted
at close-out (`git log --all -- docs/plans/<name>.md` recovers
them). The active plan is `docs/plans/query-builder.md`; its
chunk-start gates (`N.1`) walk this index, and bare chunk numbers in
`Lands:` refer to its chunks. Every other entry is
condition-triggered with a self-contained condition. Entries may
also be pulled as a proactive burn-down — each resolved as its own
change set: diagnose → fix → regression test → adversarial review →
promote-then-delete.

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

## Open — query-builder plan riders

Not defects: spec-amend clause text and design requirements
parked until the chunk that implements them (amendments to
as-built specs ride their implementation chunks). No severity
tags.

| Slug | Lands | Summary |
|------|-------|---------|

*None open.*

## Open — change-set review findings

Adjacent findings surfaced by per-chunk adversarial reviews;
pre-existing at their change set's base.

| Slug | Lands | Summary |
|------|-------|---------|
| leak-detection-gate-phrasing | when leak-detection.md §Cleanup Behavior / §Close Ordering is next amended | [L] three clauses still call the closed flag a bare `*atomic.Bool`; the composite gate (closed + txInflight) is the implemented and elsewhere-specced shape |
| check-issue-codes-unpinned | when check.go's issue emission or api-surface.md §CheckIssue is next amended | [L] the spec's "stable, machine-parseable" `CheckIssue.Code` tokens have no pinning tests; a rename changed one token with a green suite (caught in review) |
| descriptor-error-prefix | when internal/descriptor is next amended | [nit] descriptor validation errors carry a misleading `"page: "` prefix (never lived in internal/page) |
| typed-index-covervalue-set-inert | when typed-keyspaces.md §Covering is next amended | [L] typed.Index CoverValue on a SetKeyspace stores unreadable covering bytes (documented-inert); the sibling ColumnIndex form now rejects the same combination — the two typed forms disagree |

## Open — design gaps (2026-07-11 architecture audit)

| Slug | Lands | Summary |
|------|-------|---------|
| index-background-maintenance-hook | when an index kind requiring asynchronous maintenance (vector ANN, FTS stats) is designed | index write path is synchronous-only: no deferred-obligation state, no background hook, per-op cursor invalidation (no epoch model), extractor-replay Check unsound for centroid-dependent kinds |
| index-kind-payload-plumbing | when the first non-composite index kind is implemented | pinned index state / flush path rebuild registry entries from the decl and would drop a stored `KindPayload`; unreachable while the open gates reject non-composite kinds |

## Open — architecture / factoring audit

| Slug | Lands | Summary |
|------|-------|---------|

*None open — resolved issues live in git history
(`git log --all -- docs/issues/<slug>.md`).*

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

| Slug | Lands | Summary |
|------|-------|---------|
| copyto-hardlink-destination-support | when a non-hard-link destination filesystem is decided to be a supported CopyTo target | [L] publish fails on vfat/exfat/FUSE targets; [nit] NFS link() retransmission quirk |
| reclaimed-boundary-torn-peer | when grant-handoff tear detection or reclaimed-boundary gating is settled | [H] surviving handle's chain predates a peer's torn never-published reclamation; reclamation behind the reclaimed boundary double-frees |
| recovery-rewrite-identity-unverified | when a foreign/older-format writer can produce checksum-valid metas with nonzero padding | [L] gated Open's self-durable rewrite assumes encode∘decode identity; a padding-divergent meta would get a changed-bytes rewrite of the sole carrier |

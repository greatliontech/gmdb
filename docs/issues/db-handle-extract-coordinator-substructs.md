# `DB` handle inlines batch + maintenance coordinator field clusters

**Lands:** condition — proactive burn-down. Internal refactor, no
external blocker, not breaking.

## Problem

The `DB` struct (`db.go:25`) inlines two cohesive coordinator clusters
as loose fields rather than composing them as sub-structs:

- **Batch coordinator** (7 fields, `db.go:95-102`): `batchMu`,
  `batchCh`, `batchDone`, `batchCtx`, `batchCancel`, `batchStarted`,
  `batchClosed`.
- **Background maintenance** (5 fields, `db.go:110-118`): `maintCtx`,
  `maintCancel`, `maintDone`, `maintStarted`, `scrubCursor`.

These 12 fields are ~40% of the handle's field count and have a clear
single-owner lifecycle (`ensureBatchCoordinator` /
`stopBatchCoordinator`; `maintenanceLoop` / `stopMaintenance`), so they
read as two embedded components that happen to be flattened into the
top-level handle.

## Resolution

Extract `batchCoordinator` and `maintenance` structs and embed them in
`DB`. The lifecycle methods already exist and move with their state;
the change is mechanical and shrinks the handle's cognitive surface (it
also localizes the `batchMu`-guarded invariants to the type that owns
them). Purely a composition cleanup — no behavioral change.

## Notes

Surfaced during the 2026-05-30 architecture/factoring audit
(root-package composition pass). Lowest-risk item in the audit; listed
for completeness.

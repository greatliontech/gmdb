# Compact() splits the database from peer-process handles — peer writes go to the unlinked inode and are silently lost

**Lands:** audit-burndown-2026-07 chunk 14.

**Severity:** [H] — silent write loss in the multi-daemon deployment
overview.md targets: daemon A and B both Open(path); A.Compact();
B.Update(Put) succeeds but commits to the dead inode; A and every
future opener never see the row; B's reads stay frozen pre-Compact.

**Source:** 2026-07-04 full-codebase audit (bulkload/maintenance
auditor).

**Governing spec:** `docs/specs/api-surface.md` §Compact — addresses
pre-rename cross-process readers and post-rename fresh openers only;
peer writer handles are unaddressed (spec-amend required).

## Problem

`compact.go:56-132, 137-199` renames a new inode over `db.path` and
reopens only this handle. A peer's long-lived handle keeps fd/mmap on
the old, unlinked inode; the lock file is not renamed and the UUID is
preserved, so the peer's next Begin acquires the flock normally and
commits to the unlinked inode. No inode/generation revalidation exists
on the Begin/commit path (no fstat/SameFile/epoch check in `db.go`,
`tx.go`, `internal/lock`).

## Fix direction

Generation stamp in the lock-file header, bumped by Compact under the
write lock; checked at write-grant acquisition and reader Begin — a
stale-generation handle gets a distinct terminal error (poisoned,
must reopen). Pre-v1 clean break for the lock-file layout is fine
(`development: true`). Amend api-surface.md §Compact with the
generation protocol and the peer-handle contract. Regression:
cross-process test — peer handle's post-Compact Begin fails with the
generation error; no write lands on the unlinked inode.

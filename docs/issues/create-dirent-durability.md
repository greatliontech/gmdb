# No parent-directory fsync at database creation; SyncDurable acks can vanish with the file

**Lands:** audit-burndown-2026-07 chunk 10.

**Severity:** [M] — power loss after N acked SyncDurable commits on a
newly created DB can leave the file absent (fs-dependent; POSIX gives
no dirent guarantee without dir fsync).

**Source:** 2026-07-04 full-codebase audit (durability auditor).

**Governing spec:** `docs/specs/durability.md` — never mentions dirent
durability (spec gap; amend alongside).

## Problem

The create path (`db.go:169-200` → `internal/pager/init.go:114`)
fsyncs the file only; the commit path's fdatasync never persists the
dirent. The project already encodes the rule — `compact.go:201-213`
(`syncDir`, "renaming is durable only after the parent directory is
fsynced") — but not at create. Adjacent: `compact.go:126` downgrades a
syncDir failure to a warning; after a failed dir fsync, acked
SyncDurable commits on the new inode are lost if a crash resurrects
the old inode.

## Fix direction

fsync the parent directory after creating the database file (and the
lock file, same reasoning); make Compact's syncDir failure an error
(poison/fail the compact, not a warning). Amend durability.md with a
dirent-durability clause. Test: unit-level assertion that create calls
the dir-sync helper (fault-injection crash test is out of unit scope;
state the cap).

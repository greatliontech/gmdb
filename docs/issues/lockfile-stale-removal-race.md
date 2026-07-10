# Stale lock-file removal is an unguarded unlink by name: split brain

Lands: 11

## Finding

**[H] Two concurrent openers hitting a stale lock file can end up
coordinating on two different lock files — two simultaneous writers on
one data file.** `internal/lock/lock.go:158-161` (errStaleUUID arm; the
size-mismatch arm at `lock.go:309-328` has the same shape): on UUID
mismatch, `Open` runs `p.Root.Remove(p.Base)` unconditionally, though
validation was performed on an fd the name may no longer point to.
Interleaving: stale file S exists; A and B open concurrently. A adopts
S → mismatch → removes S → creates fresh L_A → coordinates on L_A. B
had already opened S by fd → sees mismatch → `Remove(Base)` unlinks
**A's L_A** → creates L_B. A holds flock/mmap on the unlinked L_A; B on
L_B; both can hold LOCK_EX simultaneously; reader tables are disjoint →
meta overwrite/page aliasing corruption. Reachable whenever a
UUID-mismatched or old-layout lock file is present and two processes
open concurrently (e.g. a database recreated at the same path).

## Fix direction

Verify identity before unlink: fstat the validated fd vs a fresh stat
of the path (same inode+dev) and remove only on match — or perform the
remove while holding a lock on the validated fd and re-validate. After
create/adopt, verify the opened fd still matches the path before
coordinating. Spec-amend rider: cross-process.md §Lock File Lifecycle
defines recreate for UUID mismatch only and has no version-handling
clause; the size-mismatch⇒stale-recreate arm and its safety invariant
("layout change ships with a data-format break") live only in a code
comment — both belong in the spec (surfaced in the audit spec-amend
list).

## Provenance

2026-07-10 defect audit; cross-process reviewer.
`TestOpenUUIDMismatchRecreates` is sequential; the concurrent-open
tests exercise create-vs-adopt, not concurrent stale-removal.

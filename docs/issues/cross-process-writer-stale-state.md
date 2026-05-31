# Cross-process writers corrupt the file: in-memory meta/bitmap/RPL never re-synced after a peer commits

**Lands:** proactive — **scope decision resolved 2026-05-31: multi-process
writing IS in v0 scope → implement the fix** (see Scope decision below). The
bug is a confirmed [H]; the resolution is the re-sync fix, not a documented
limitation.

**Severity:** [H]

**Source:** 2026-05-30 deep audit (run `wf_4ad12a2f-039`), raw findings 6
and 8 — two finders (algorithm-design + concurrency-mvcc) converged.

**Governing spec:** `docs/specs/cross-process.md` (the ~950-line
cross-process coordination contract); `overview.md` Design Decisions
("cross-process per-keyspace concurrent writers… single shared meta
root"); `api-surface.md:1555` (Compact's "writers in other processes are
blocked" contract).

## Problem

The write path reads the active meta, the allocation bitmap, and the RPL
chain into per-process memory **only at `Open`** and never re-syncs them
after a *peer* process commits. The flock (`LOCK_EX`) serializes writers,
but a writer that acquires the grant trusts its own stale in-memory state.

Sequence (two handles — two processes, or two `gmdb.Open` handles in one
process — on one file; both start at `currentMeta.TxnID=5`,
`activeMetaIdx=0`):

1. Process A: `Begin → Put(k1) → Commit` publishes disk meta `TxnID=6` to
   slot 1, fsyncs, releases the flock. A now holds `meta=6/idx=1`; disk is
   `slot0=5, slot1=6`.
2. Process B: `Begin` acquires the flock, but its in-memory state is still
   `meta=5/idx=0` (read at Open, never refreshed). B computes
   `newTxnID = prevMeta.TxnID+1 = 6`, builds its tree on the `TxnID=5`
   roots (**A's `TxnID=6` changes invisible → A's commit silently lost**),
   allocates from a **stale bitmap** (handing out pages A's committed tree
   references → **page aliasing / corruption**), and writes its meta to
   slot `1-0 = 1`, **clobbering A's `TxnID=6` meta**.
3. The pager guard `cp.NewTxnID <= prev.TxnID` (`commit.go:86`) uses B's
   stale `prev` (`TxnID=5`), so it passes. Result: `k1` permanently lost;
   the on-disk file now has two metas both claiming `TxnID=6` with
   divergent trees; B's bitmap will keep handing out pages A's tree
   references.

The same gap hits reads: `BeginRead` (`read_tx.go:258`) also reads
`db.currentMeta`, so a reader in process B can never observe process A's
commits and stamps a stale `TxnID` into the cross-process reader table,
mis-driving the writer's reclamation bound.

This is guaranteed lost-update + corruption for any two concurrent
cross-process writers — the exact scenario the stale-writer-recovery /
`WriterPID`-handoff machinery and the Compact "writers in other processes
are blocked" contract presuppose works.

Code anchors: `db.go:570-571` (write `Begin` reads `db.currentMeta` /
`activeMetaIdx`); `db.go:257` + `tx.go:375-376` (only writers of
`currentMeta`); `internal/pager/init.go:290-308` (bitmap copied out of
mmap + RPL chain rebuilt — **only at Open**); `internal/pager/commit.go:79,106`
(Commit trusts the caller's `prev`/`prevActive`, writes to `1-prevActive`).

## Fix

After acquiring the write grant in `db.Begin` (and at `BeginRead`),
re-read both meta pages from the mmap/file, re-run the
checkpoint-preferring active-meta selection (the exact logic
`pager.Open` already runs), and adopt the result as
`prevMeta`/`prevActive`. Because the bitmap and RPL chain are also
per-process and stale, when the freshly-read meta `TxnID` differs from the
cached one, rebuild (or revalidate) the in-memory `Bitmap` from the
on-disk bitmap region and the RPL chain from `RPLHeadPage`/`RPLTailPage`.
Cheap when unchanged: compare `TxnID` and skip. The `reopenAfterCompact`
path already does this full reload — factor it.

## Scope decision (resolved 2026-05-31)

**Decided: concurrent/serialized cross-process writing IS in v0 scope → fix
the bug** (re-sync on write-grant acquisition, per the Fix section above).
This matches the spec's evident intent — the entire lock-file / reader-table
/ heartbeat / WriterPID-handoff / stale-writer-recovery subsystem exists to
support a writing process that changes over the file's life (serialized via
the flock, including failover after a holder dies). The `Fix` is therefore a
real correctness fix, not a documentation-only limitation. A spec gap remains
to close as part of it: `cross-process.md §Writer acquisition flow` covers the
lock handoff but never states the re-sync-meta-on-grant step the multi-writer
contract requires — amend it.

## Verification

End-to-end test with two simultaneously-open handles on one file (see
the companion issue `cross-process-coordination-untested`): handle B
begins a tx after handle A commits → B sees A's data; interleaved writes
A,B,A,B → on-disk `TxnID` strictly increases and no key is lost. Run
under `-race`. This is the acceptance gate for the fix.

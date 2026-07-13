# Integrity and Safety

A consolidated summary of the engine's safety properties — each
mechanism is specified in its dedicated spec, this file is the
index a reader (or auditor) can scan to confirm the engine
covers a given failure mode.

Scope:
- No partial writes visible (CoW + meta swap).
- Atomic commit.
- Write ordering.
- Reader isolation.
- mmap protections.
- Stale reader / writer recovery.
- Index consistency.
- Silent bitrot detection.
- Forged / structural corruption tolerance (read path).
- Disk-full (ENOSPC) handling.

## Invariants

This spec records no invariants of its own — it is an index over
the safety properties stated and enforced in:

- `pager-slab.md` — CoW + slab buffer ownership + commit write
  ordering + bounded crash leakage.
- `file-layout.md` — dual meta + active-meta selection.
- `free-space.md` — bitmap/RPL invariants and reclamation
  bound.
- `cross-process.md` — reader-table and writer-header
  invariants, stale recovery.
- `mmap-strategy.md` — `PROT_READ` enforcement.
- `durability.md` — durable sub-record + recovery rule.
- `indexing.md` — atomic row + index maintenance.
- `checksums.md` — silent bitrot detection.

A reader auditing safety should treat each bullet below as a
pointer into the relevant spec; the *enforced* invariants live
there.

## No partial writes visible

CoW ensures all modifications happen on new pages. The old tree
is intact until the meta-page swap. Bitmap leakage (pages that
appear allocated but are unreferenced) is possible on crash
between the bitmap pwrite and the meta pwrite, but tree
integrity is always preserved. See `pager-slab.md §Commit Write
Ordering`. Leaked pages are detected by `Check` (the BitmapLeak
finding) and reclaimed offline, under exclusive access, by
`CheckWithOptions` with `Repair` set — see `api-surface.md
§CheckOptions`.

## Atomic commit

A single meta-page write (< page size, aligned) is the commit
point. Even if torn, the checksum fails and the DB falls back to
the other meta page. See `file-layout.md §Meta Page` and
`checksums.md §Meta Page Checksum`.

## Write ordering

In `SyncDurable`, data + bitmap pwrites are fdatasync'd BEFORE
the meta page write, and the meta is fdatasync'd AFTER. In other
sync modes, ordering relies on CoW and the durable sub-record
(recovery adopts only fsync-covered state — see `durability.md`).
See `pager-slab.md §Commit Write Ordering`.

## Reader isolation

Readers see an immutable snapshot. Pages they reference cannot be
reused until all readers on that TxnID have finished. See
`transactions.md §Read Transaction` and `free-space.md §RPL
Reclamation`.

## mmap is `PROT_READ`

A stray pointer in the host process produces SIGSEGV rather than
silently corrupting the file. The writer's mutations live in
slab buffers (process memory), where unsafe-pointer bugs can
still cause harm — but they cannot reach disk except via the
controlled pwrite path. See `mmap-strategy.md`.

## Stale reader recovery

If a process crashes without releasing its reader slot, the PID
liveness check + process start time comparison allows the writer
to reclaim the slot — even if the PID has been recycled.
Cross-PID-namespace cases fall through to the heartbeat path.
See `cross-process.md §Reader Table` and §Stale Reader
Detection.

## Stale writer recovery

If the writer process crashes, the kernel releases the flock
automatically. The next writer detects the dead or recycled PID,
cleans up reader slots from the crashed process, and proceeds —
CoW guarantees the tree is consistent. Bitmap integrity is
guaranteed by the deferred-pwrite approach: bitmap modifications
are held in memory (`tx.pendingAllocs` / `tx.pendingFrees`) and
only written to disk via pwrite at commit time. If the writer
crashes before commit, no bitmap modifications reach disk — no
leaked pages. Slab buffers in anonymous mmap are released to the
OS on process exit; no on-disk artifacts. See `cross-process.md
§Stale Writer Recovery`.

## Index consistency

Every index update happens in the same CoW transaction as the
row write. Either both succeed or both are rolled back. Index
drift can only occur if the user changes the extractor without
bumping `Version` (or vice versa) — caught at Open by the
schema-hash + version fingerprint check; the engine refuses to
open the keyspace until `RebuildIndex` is called. See
`indexing.md §Write Path` and §Drift Guard.

## Silent bitrot detection

When `PageChecksum` is enabled (the default), every data page
read is verified against its XXH3-64 footer on first access in a
transaction (cached thereafter). Corruption is detected at read
time with `ErrBadPageChecksum` identifying the affected page. See
`checksums.md §Verification`.

## Forged / structural corruption tolerance

Checksum verification catches accidental bitrot but not a
deliberately-forged page (recomputed footer) or a
checksum-disabled database. The read path is independently
hardened so a corrupt or forged on-disk surface yields an error
rather than a crash: content-derived page ids are bounded against
the file-resident extent before any mmap access (out-of-range ⇒
`ErrCorrupted`, never a SIGBUS on the `MaxSize` reservation gap);
allocations sized from on-disk length/count fields (overflow
`TotalLen`, index-registry counts) are bounded before they are
made (no OOM); and branch pages are structurally validated before
their directory is iterated (no out-of-bounds panic). See
`checksums.md §Structural and Allocation Bounds`.

## Disk full (ENOSPC)

If `ftruncate()` (growth) or `pwrite()` (data / bitmap / meta)
fails with ENOSPC, the operation returns an error. A failed
`pwrite()` during commit may result in a partially written
page on disk. Since the meta page has not been updated,
recovery falls back to the previous meta — the partially
written pages are superseded by the next successful commit.
File-growth failures during the transaction cause
`pageAlloc()` to return `ErrDBFull`. Slab-buffer pwrite
failures during commit abort the commit (the transaction must
be rolled back at the application level; the on-disk state is
consistent with the previous meta). See `pager-slab.md
§Commit-failure cleanup` and `file-format.md §File Growth`.

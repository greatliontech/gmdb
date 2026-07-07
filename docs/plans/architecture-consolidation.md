# gmdb architecture consolidation & recovery-model redesign

Chunks 1–10 are governed by `docs/specs/durability.md`,
`free-space.md`, `cross-process.md`, `file-layout.md`; chunks 11–18
are behavior-preserving consolidation under the specs each touched
area cites. Chunks in dependency order; sub-chunks `N.1` (triage
gate) and the final close-out are fixed anchors per chunk.

## Phase A — commit/recovery/RPL groundwork (behavior-preserving)

- [x] 1. Unify the RPL chain walkers: one pager-owned on-disk chain
  walker carrying the shared tail-terminated / reclaimed-boundary /
  head-exemption policy, consumed by `rebuildRPLChain`
  (`internal/pager/init.go:656`) and `checker.walkRPL`
  (`check.go:675`), plus a shared segment read/validate
  (footer-verify + decode) primitive also consumed by `reclaimRPL`
  (`internal/pager/freespace.go:472`), which walks the in-memory
  list per the spec.
- [ ] 2. Collapse meta selection to one seam: deduplicate the
  tie-break cascade in `page.ActiveMeta` /
  `page.ActiveMetaCheckpointPreferring` (`internal/page/meta.go:195,
  276`), fold `highestCheckpointTxnID`
  (`internal/pager/init.go:484`), and extract the shared
  read/decode/validate helper for `readAndSelectMeta` / `Resync` /
  `ReadLatestMeta` (`internal/pager/init.go:244,447,509`).
- [ ] 3. Centralize root resync adoption and the reclamation bound:
  one adoption helper for the `currentMeta` / `activeMetaIdx` /
  `lastCheckpointTxnID` triple (6 update sites) and one bound helper
  owning `min(oldestReaderTxnID, lastCheckpointTxnID)` plus the
  no-reader sentinel (`db.go:809,853–875`, `check.go:174`).
- [ ] 4. Collapse the pager write-tx seeding ritual into
  `BeginTx(TxParams)` (today 7 order-dependent setters,
  `db.go:810–879`); inject the reclamation-bound source as a
  dependency, retiring `SetReclamationBoundRefresh`.
- [ ] 5. Extract a write-grant facade on `DB` for the triplicated
  grant → poison/generation re-check → `Resync` preamble in `Begin` /
  `Checkpoint` / `Compact` (`db.go:717`, `checkpoint.go:66`,
  `compact.go:66`), deduplicating the poison-log path.
- [ ] 6. Unify top-level tx abort with savepoint restore (implicit
  depth-0 savepoint; `internal/pager/pager.go:459–557` vs
  `internal/pager/savepoint.go:200–573`); group the loose `Pager`
  tx-snapshot and cold-tracking fields into sub-structs; delete
  `bitmapwrap.go`.

## Phase B — recovery-model redesign (Spec-first)

- [ ] 7. Design: amend `durability.md`, `file-layout.md`,
  `free-space.md`, `cross-process.md` to highest-valid-epoch recovery
  with an on-disk `durableEpoch` marker; settle the multi-process
  questions (cross-process visibility under deferred meta,
  peer-visible durable-epoch marker); disposition
  `docs/issues/rpl-head-exemption-reclaimed.md` and
  `docs/issues/rpl-segment-relocation.md` inside the design.
- [ ] 8. Implement `durableEpoch` + highest-epoch recovery; retire
  checkpoint-preferring selection, the live-visibility-vs-recovery
  meta-selection asymmetry, and the genesis-rollback special case.
- [ ] 9. Retire/reshape the `lastCheckpointTxnID` reclamation-bound
  machinery per the amended spec.
- [ ] 10. Land the chunk-7 dispositions of the RPL head exemption and
  RPL segment relocation.

## Phase C — independent collapses

- [ ] 11. btree: extract the shared merge/redistribute pair dispatch
  (3 copies: `internal/btree/delete.go:491–704,799–901`,
  `internal/btree/range_delete.go:659–892`).
- [ ] 12. btree: descent-skeleton unification — shared per-level
  branch-descend helper; collapse `walk.go`'s three walkers and
  `cursor.go`'s five `descend*` variants.
- [ ] 13. btree: unify `PutEntry` (`entry_ops.go:110`) with
  `putReportCore` (`put.go:140`); shared root-collapse / final-heal
  for `Delete` / `DeleteRange` (`delete.go:90–128` ≡
  `range_delete.go:145–181`); merge `pathFrame` / `cursorFrame`.
- [ ] 14. page: leaf codec dedup — shared full-key entry decoder
  (`decodeRestartEntry` ≡ `ucDecodeEntry`), shared entry writer
  (`writeUCEntry` ≡ `writeCompressedRestartEntry`; the
  evolve-independently hedge is dropped), unified restart/uc
  validators (`internal/page/leaf.go:545,639`).
- [ ] 15. lock: shared reader-slot clear body (`ReleaseReaderSlot` ≡
  `ClearStaleReaderSlot`, `internal/lock/reader.go:114,164`); shared
  PID/namespace liveness classification (`recovery.go:38` vs
  `reader.go:308`); doc fixes (stale header size in `doc.go`,
  orphaned `readerSlotHint` comment in `coord_reader.go:20`).
- [ ] 16. root: resolve the `compact.go` (rebuild-and-swap) vs
  `compaction.go` (online incremental relocation) naming collision;
  extract the repeated coord-snapshot idiom.
- [ ] 17. root: shared tx-guarded cursor core embedded by `Cursor`
  and `SetCursor`'s outer half; collapse the `txCounters` →
  `TxStatsSnapshot` field-copy boilerplate
  (`internal/pager/txstats.go:92`).
- [ ] 18. page: split node formats (leaf/branch/subpage/overflow)
  from pager-domain formats (`meta.go`, `rpl.go`,
  `keyspace_descriptor.go`) over a shared wire/header base.

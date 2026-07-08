# gmdb architecture consolidation & recovery-model redesign

Chunks 1–9 are governed by `docs/specs/durability.md`,
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
- [x] 2. Collapse meta selection to one seam: deduplicate the
  tie-break cascade in `page.ActiveMeta` /
  `page.ActiveMetaCheckpointPreferring` (`internal/page/meta.go:195,
  276`), fold `highestCheckpointTxnID`
  (`internal/pager/init.go:484`), and extract the shared
  read/decode/validate helper for `readAndSelectMeta` / `Resync` /
  `ReadLatestMeta` (`internal/pager/init.go:244,447,509`).
- [x] 3. Centralize root resync adoption and the reclamation bound:
  one adoption helper for the `currentMeta` / `activeMetaIdx` /
  `lastCheckpointTxnID` triple (6 update sites) and one bound helper
  owning `min(oldestReaderTxnID, lastCheckpointTxnID)` plus the
  no-reader sentinel (`db.go:809,853–875`, `check.go:174`).
- [x] 4. Collapse the pager write-tx seeding ritual into
  `BeginTx(TxParams)` (today 7 order-dependent setters,
  `db.go:810–879`); inject the reclamation-bound source as a
  dependency, retiring `SetReclamationBoundRefresh`.
- [x] 5. Extract a write-grant facade on `DB` for the triplicated
  grant → poison/generation re-check → `Resync` preamble in `Begin` /
  `Checkpoint` / `Compact` (`db.go:717`, `checkpoint.go:66`,
  `compact.go:66`), deduplicating the poison-log path.
- [x] 6. One snapshot capture: `snapshotCore` + `captureCore` shared
  by the top-level tx snapshot (`Pager.txSnapshot`, replacing four
  loose fields) and `Savepoint` (embeds it); restore policies
  deliberately remain distinct (wholesale abort vs undo-log replay —
  `snapshotCore`'s doc carries the why); `coldTracker` sub-struct;
  `bitmapwrap.go` deleted.

## Phase B — recovery-model redesign (Spec-first)

- [x] 7. Design (user-selected durable-sub-record model): amend
  `durability.md`, `file-layout.md`, `free-space.md`,
  `cross-process.md`, `transactions.md`, `background-maintenance.md`
  — every meta carries its recovery target (the durable sub-record:
  `DurableTxnID` + the epoch's state-bearing fields incl. the
  persisted RPL head TxnID); recovery = one selection (highest valid
  TxnID, shared with live paths) + the durable projection; the
  checkpoint flag, checkpoint-preferring selection, and the
  sustained-SyncLazy unsafe degradation retire; reclamation bound =
  `min(oldestReader, anchoredEpoch)` (`AnchoredDurableTxnID`, the
  newest fsync-covered assertion). Dispositions:
  rpl-head fix designed (head exemption conditioned on epoch
  ownership via persisted `RPLHeadTxnID`), lands chunk 8;
  rpl-segment redeferred unchanged (mechanics untouched).
- [x] 8. Implement the amended contract (format-version bump, clean
  break): meta codec + `RPLHeadTxnID` + durable sub-record +
  `AnchoredDurableTxnID`; commit/Checkpoint sub-record and
  anchoring maintenance; recovery = durable projection + the
  recovery commit; head-exemption `RPLHeadTxnID >= DurableTxnID`
  rule in the shared walker; retire `MetaFlagCheckpoint`,
  `ActiveMetaCheckpointPreferring`, `HighestCheckpointTxnID`, the
  `NoCheckpoint` plumbing, and the genesis special case.
- [x] 9. Root-side retirement + clean shutdown: delete
  `db.lastCheckpointTxnID` (bound from the anchored epoch);
  writable `Close()` performs the Checkpoint sequence; rewire
  `reclamationBound`, `setMetaState`/`adoptOpened`, Checkpoint
  publish, the maintenance first-pass trigger, and stats/warn
  surfaces; update docs and the recovery test nets.

## Phase C — independent collapses

- [x] 11. btree: extract the shared merge/redistribute pair dispatch
  (3 copies: `internal/btree/delete.go:491–704,799–901`,
  `internal/btree/range_delete.go:659–892`).
- [x] 12. btree: descent-skeleton unification — shared per-level
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

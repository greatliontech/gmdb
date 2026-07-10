# Defect-audit remediation (2026-07-10 wave)

Spec: docs/specs/ (per-chunk governing spec named in each issue doc).
Issues: docs/issues/README.md §2026-07-10 wave — one issue doc per
chunk; the issue carries the defect detail, failure scenarios, fix
direction, and spec-amend riders. Bottom-up by layer, grouped by
function. WIP = 1.

- [x] 1. internal/page: uncompressed-leaf read path — exact-match-last
      seek panic, iterator Prev/At/Next desync, iterator doc contracts
- [x] 2. internal/btree: delete-side re-encode growth — native-variant
      splice fallback for delete/range-delete rebuilds, in-place ref
      patch for relocate (+ page-formats.md rider)
- [x] 3. internal/btree: rebalance termination — leaf redistribute
      fill-floor decline, deep-underflow heal propagation
      (+ range-delete.md rider)
- [x] 4. internal/btree: read-path validation — cousin-scan first-read
      validation, Cursor.Delete reposition error surfacing
- [ ] 5. internal/btree + set keyspace: subpage promotion multi-leaf
      build (+ set-keyspace.md rider)
- [ ] 6. internal/btree + internal/page: compact fixed-size nested-leaf
      cells per set-keyspace.md
- [ ] 7. internal/pager: crash-coherent RPL reclamation — neutralize
      half-reclaimed segments at Open (+ free-space.md rider)
- [ ] 8. internal/pager: file-resident bounds — writer fileSize
      tracking, reader shrink-stable bound, MaxSize clamp
- [ ] 9. internal/pager: commit residue — checkpoint anchor advance,
      rplRelocFloor lifecycle, relocation probe segment projection
- [ ] 10. internal/lock: validated stale-clear — occupancy re-check
      before clear, acquire-window heartbeat/publish hardening,
      recovery-gate comment (+ cross-process.md rider)
- [ ] 11. internal/lock: lock-file identity guard — inode-verified
      stale removal, post-create identity re-check
      (+ cross-process.md rider)
- [ ] 12. internal/lock: boot-epoch discriminator — invalidate
      cross-boot stamps/start-times, lock-file lifecycle clause
      (+ cross-process.md rider)
- [ ] 13. batch: Goexit isolation, post-acceptance ctx contract,
      cascade reserve re-price, self-commit outcome doc
      (+ transactions.md rider)
- [ ] 14. tx: child SetFileFormat merge, View error join, iterator
      guard-error surfacing
- [ ] 15. db: daemon goroutines stop pinning *DB — handle-leak
      detection reachable; LaggingReader reentrancy doc
- [ ] 16. keyspace: nested delete+recreate kills the parent's old
      handle (both kinds)
- [ ] 17. indexing: onDelete extract-all-then-mutate, coverValue
      reconcile, schemaHash doc grammar
- [ ] 18. bulkload: index-entry parity — key gate, overflow promotion,
      base config for index trees (+ bulkload.md rider)
- [ ] 19. copy/check: verbatim-walk clamp, temp+rename destination,
      overflow-header validation (+ api-surface.md/checksums.md riders)
- [ ] 20. maintenance: leak reclamation gated on tail-reached RPL walk
      (+ background-maintenance.md rider)
- [ ] 21. compaction: below-floor allocation for tree-page relocations
- [ ] 22. specs: descriptive-drift sync batch

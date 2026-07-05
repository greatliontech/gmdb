# gmdb 2026-07 audit burn-down plan

Fix roadmap for the 2026-07-04 full-codebase audit (5 subsystem
auditors; findings filed as `docs/issues/*` rows tagged "2026-07-04
audit"). Chunks in dependency order, bottom-up by layer. Each chunk
resolves its issue doc(s) as one reviewed change set (diagnose → fix +
regression test → adversarial review → close-out gate → commit) per
`~/.claude/CLAUDE.md`. WIP = 1. Clean-break stance of
`docs/plans/v0-implementation.md` applies (`development: true`).

- [x] 1. page-compressed-leaf-sharedlen-validation
- [x] 2. btree-delete-separator-branch-overflow
- [x] 3. btree-retired-pages-rollback
- [x] 4. pager-rpl-footer-verification
- [x] 5. pager-freed-page-write-skip
- [x] 6. lock-stale-slot-clear-identity
- [x] 7. reader-begin-publish-race
- [x] 8. lagging-reader-bound-checkpoint-term
- [x] 9. checkpoint-failure-poisoning
- [x] 10. create-dirent-durability
- [x] 11. beginread-close-lifecycle
- [x] 12. update-unresolved-child-grant
- [x] 13. maintenance-reclaim-snapshot-guard
- [x] 14. compact-peer-handle-generation
- [x] 15. check-consistency-classes
- [x] 16. iterator-cursor-unregistration
- [x] 17. index-covering-value-diff
- [x] 18. index-child-merge-handle-reconciliation
- [ ] 19. readonly-index-lookups
- [ ] 20. setkeyspace-bulkload-error-mapping
- [ ] 21. set-cursor-materialization-bound
- [ ] 22. api-and-doc-drift-sweep

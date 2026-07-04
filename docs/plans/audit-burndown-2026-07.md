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
- [ ] 7. reader-begin-publish-race
- [ ] 8. lagging-reader-bound-checkpoint-term
- [ ] 9. checkpoint-failure-poisoning
- [ ] 10. create-dirent-durability
- [ ] 11. beginread-close-lifecycle
- [ ] 12. update-unresolved-child-grant
- [ ] 13. maintenance-reclaim-snapshot-guard
- [ ] 14. compact-peer-handle-generation
- [ ] 15. check-consistency-classes
- [ ] 16. iterator-cursor-unregistration
- [ ] 17. index-covering-value-diff
- [ ] 18. index-child-merge-handle-reconciliation
- [ ] 19. readonly-index-lookups
- [ ] 20. setkeyspace-bulkload-error-mapping
- [ ] 21. set-cursor-materialization-bound
- [ ] 22. api-and-doc-drift-sweep

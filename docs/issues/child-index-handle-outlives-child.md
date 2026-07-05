# Child-created IndexHandle iter closures outlive the child transaction

**Lands:** when the IndexHandle iter closures gain the requireOpen
guard that Stats already has (index.go — Lookup/LookupKeys/Range/
Prefix/Get closures), or with the chunk-22 API/doc sweep if pulled
there.

**Severity:** [M] — after a child COMMIT, a handle obtained from the
child's own OpenKeyspace keeps serving lookups (2 rows, Err()==nil in
the reviewer's reproducer) even though the child tx is closed and the
BeginChild contract promises ErrTxClosed on every child handle; after
a child ROLLBACK the same handle descends savepoint-reverted pages —
ErrCorrupted ("page N unexpected type 0") on a healthy database, or
silently wrong data if the freed page parses as a valid leaf.

**Source:** 2026-07-05 adversarial review of the chunk-18 change set
(index-child-merge-handle-reconciliation), question 1(c). Reproducer:
the reviewer's out-of-tree module (child handle used after child
Commit and after child Rollback). Cause-line predates chunk 18: the
iter closures lack the `requireOpen` probe `Stats` performs
(index.go Stats vs the Lookup closure).

**Governing spec:** `docs/specs/indexing.md` §Handle Invalidation
(Inv-IHS4 documents the END state of parent handles; the child
handle's end state is the BeginChild contract in nested.go: every
child handle errors ErrTxClosed once the child ends).

## Fix direction

Add the requireOpen(false) probe to every IndexHandle iter closure's
entry (and Get), mirroring Stats — the closure's first action should
surface ErrTxClosed via the sticky err. Regression: child handle
after Commit → ErrTxClosed; after Rollback → ErrTxClosed (never
ErrCorrupted/freed-page reads).

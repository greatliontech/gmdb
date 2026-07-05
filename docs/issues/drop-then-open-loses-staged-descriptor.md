# Same-tx index Drop on a not-open keyspace is lost by a subsequent open

**Lands:** when the open paths preserve (or merge) a staged
dirty-descriptor entry instead of discarding it — the
`delete(tx.dirtyDescriptors, name)` in Tx.OpenKeyspace /
OpenKeyspaceReadOnly / OpenSetKeyspace / OpenSetKeyspaceReadOnly must
not drop state staged by TxIndexes.Drop (and audit the
SetKeyspaceConfig-then-open family for the same pattern).

**Severity:** [H] — silent data corruption: TxIndexes.Drop on a
keyspace NOT currently open stages the updated descriptor in
tx.dirtyDescriptors and FreeSubtree's the index pages; a subsequent
open in the same tx caches the handle as Clean and deletes the
staged entry. flushKeyspaces skips Clean handles, so at commit the
descriptor write never lands while the page frees do — the registry
entry RESURRECTS with its root pointing at freed pages (reviewer's
overlay probe: Drop → RO-open → Commit → fresh tx Index() returns a
live-looking handle over a dangling root; control without the open
passes).

**Source:** 2026-07-05 adversarial review of the chunk-19 change set
(readonly-index-lookups), same-tx interaction question. Cause-lines
predate the chunk (reproduces on base via the write-open variant);
the RO-lookup fix merely made it observable through RO handles.

**Governing spec:** `docs/specs/indexing.md` §Index Administration
(Drop's contract); `docs/specs/keyspaces.md` descriptor flush rules.
The spec is silent on Drop/open same-tx ordering — the fix should
also state it.

## Fix direction

The open paths' dirty-descriptor discard is correct for the
REOPEN-refresh case it was built for but wrong when the staged entry
came from an admin op on an uncached keyspace. Either seed the opened
handle's state from the staged descriptor (open sees the
post-Drop descriptor and marks the handle dirty), or key the discard
to provenance. Regression: Drop → open (all four variants) → Commit
→ reopen: index must stay dropped, Check clean.

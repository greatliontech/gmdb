# Range Delete

`Keyspace.DeleteRange(start, end)` deletes all keys in `[start, end)`
in a single operation. On un-indexed keyspaces this is significantly
more efficient than cursor iteration because it retires entire
subtrees without visiting individual leaves. On indexed keyspaces it
falls back to a per-row walk — see Bulk Operations below and
`indexing.md §Bulk Operations on Indexed Keyspaces`.

Scope:
- Three-phase range-delete algorithm on un-indexed keyspaces.
- Complexity comparison vs. cursor loop.
- Set-keyspace bulk-free interaction.
- Cursor-based delete loop for callers that need finer control.

Depends on / interacts with:
- `page-formats.md` for branch and leaf structure.
- `free-space.md` for the retire-pages-to-RPL hook used at the end
  of each phase.
- `set-keyspace.md` for nested-tree bulk free.
- `indexing.md` for the per-row-walk fallback contract on indexed
  keyspaces.
- `transactions.md` for cursor stability across CoW + rebalance.

## Invariants

Invariant: kind=clause-explicit;
  property=`DeleteRange(start, end)` deletes every key `k` with
    `start <= k < end` from the keyspace and zero keys outside that
    interval; `end` itself is never deleted. Passing
    `(nil, nil)` deletes every key in the keyspace. `nil` is the
    open-boundary sentinel: `nil` start = "from the beginning";
    `nil` end = "through the last key". A non-nil zero-length
    boundary (`[]byte{}`) is rejected with `ErrKeyEmpty` per
    api-surface.md §Invariants empty-key clause — distinct from
    the open-boundary `nil`;
  from=this spec §Algorithm + API contract + api-surface.md
    §Keyspace API DeleteRange;
  violation=Off-by-one in the boundary-leaf cleanup deletes `end`
    (inclusive bug) or leaves the first key `>= start` alive
    (exclusive bug) — silent data loss or silent retention of data
    the caller said to delete; OR coalescing `[]byte{}` with `nil`
    silently accepts an invalid boundary the spec rejects.

Invariant: kind=entailed;
  property=After `DeleteRange` commits, every overflow run referenced
    by any deleted entry is retired (its page IDs appear in
    `tx.retiredPages`). No overflow page survives a delete of its
    only referencing leaf entry;
  from=entailed: tree-integrity + free-space accounting
    (`page-formats.md` + `free-space.md`);
  violation=Orphan overflow runs become permanent bitmap leakage
    that background maintenance cannot reclaim until the
    leak-reclamation pass identifies them — for large value
    workloads this is unbounded leakage in practice.

Invariant: kind=entailed;
  property=A successful `DeleteRange` leaves the keyspace's B+tree
    well-formed: branch separators still satisfy
    `max(left) < S <= min(right)`, no branch has fewer than the
    minimum children except via root collapse, and the keyspace
    descriptor's `Root` points to the new root (or `0` for an
    emptied keyspace);
  from=entailed: tree invariants of `page-formats.md` + this spec
    §Phase 3 root collapse;
  violation=A separator that no longer satisfies the ordering
    invariant after boundary rebalance routes subsequent Get/Cursor
    operations to the wrong subtree; a forgotten root-collapse leaves
    an empty branch with one child, breaking the depth-derivation
    convention.

Invariant: kind=entailed;
  property=On an indexed keyspace, `DeleteRange` deletes every
    secondary-index entry that the engine's extractor would produce
    for any deleted row — exactly the same set of index entries a
    per-row `Delete()` loop would remove;
  from=entailed: per-row walk fallback (this spec + `indexing.md`);
  violation=A subtree-retirement shortcut on indexed keyspaces drops
    a leaf's index-bearing entries without removing the index rows
    — the index returns stale primary keys that point at deleted
    rows.

Invariant: kind=clause-explicit;
  property=After a successful `DeleteRange` returns, every non-root
    page reachable from the new root has fill `>= MergeThreshold%` of
    the page's `ContentEnd` **where that floor is reachable**. The root
    is exempt — a partially-emptied tree's root may shrink arbitrarily
    (and may root-collapse to a leaf or to 0 / empty). The same floor
    holds for single-key `Delete`.
    **Two size notions (load-bearing).** For a **branch** page the floor
    is measured on **logical (uncompressed) content** — the bytes its
    separators would occupy with no within-page prefix truncation
    (`page.BranchLogicalSize`) — NOT its physical compressed size.
    Within-page branch prefix truncation (`page-formats.md` §Branch
    Page) stores a page's shared separator prefix once, so a
    maximally-dense same-cluster branch carries high fan-out yet few
    physical bytes; measuring the floor on compressed bytes would
    spuriously flag it as underfull. The physical (compressed) size is
    used only for **capacity** ("does a cell set fit one page?"); the
    floor, the underflow trigger, and the redistribute split-balance all
    use logical content. For a **leaf** page the floor is physical fill
    (leaf restart-group compression keeps byte-fill representative
    because leaves also store values).
    **Reachability qualifier.** The floor is reachable — and so
    enforced — for the common case AND for deep-shared-prefix branches:
    within-page truncation packs many such separators per page (high
    fan-out, high logical content), so they land above MT, not at
    fanout-2. It is **NOT** reachable in two residual cases. (a) A branch
    reduced to a **single near-§Maximum-Key-Size separator**: no feasible
    split's logical content reaches MT (e.g. one ~1400-byte separator at
    MergeThreshold 50), and because a branch redistribute **lifts** the
    boundary separator to the parent, both halves can fall below MT. (b) A
    **cluster-seam branch** — a large within-cluster separator plus a tiny
    cross-cluster one — whose neighbours are dense same-cluster branches:
    absorbing more cells would un-compress across the cluster boundary and
    overflow a physical page, so no merge (combined > one page) and no
    redistribute (every physically-fitting split leaves the seam half
    logically below MT) can raise it. A page that no adjacent merge or
    floor-clearing redistribute can heal is **accepted below-floor**, and
    the rebalance MUST terminate rather than loop attempting to heal it.
    Where reachable, the floor is maintained by the merge/redistribute
    contract: when a local merge produces a still-below-MT page,
    `rebalanceSurvivors` re-attempts merge with the next adjacent
    survivor; a branch redistribute balances on **logical** content under
    the physical-fit constraint (so it never piles the cheap same-cluster
    cells on one half and strands the other below MT), and one that still
    cannot clear the floor for both halves **declines** (changes nothing)
    so the deficit is not relocated to a sibling. The decline contract
    holds for **leaf pairs identically**: a leaf redistribute's
    byte-balanced split is entry-granular, so a single near-page inline
    entry can strand the other half below MT — that redistribute
    declines; and a pair whose combined set has **no feasible two-page
    partition at all** (a variant-migrated delta-heavy input canonically
    inflates past two pages — the same non-monotonicity as
    `page-formats.md` §Insert and Delete's delete-side rebuild fallback)
    also declines rather than failing: infeasibility on a valid pair is
    an accepted-below-floor outcome, never an error. In every decline
    the underflowing child
    is threaded upward as `deepUnderflowChild` for a higher level (with
    more cousins) to heal, or accepted below-floor if unreachable
    everywhere — and the thread is handled on **every** pair outcome at
    the receiving level: a merge cousin-rebalances inside the merged
    result, a redistribute cousin-rebalances inside whichever output
    received the deep child (the count-balanced split decides which —
    the holder is discovered, never assumed), and a decline re-threads
    the unchanged wrapper upward. Two thread hygiene rules: (i) the id
    a level threads upward must be **live in that level's final
    returned topology** — a level's own post-decline re-rebalance can
    merge the recorded wrapper away (and a partial heal can have
    already merged the original deep itself), so a stale thread is
    reconciled by **meaning, not identity**: the final topology's
    branch children are rescanned for any still-below-floor
    grandchild; the first found per child is healed (the cousin walk
    absorbs adjacent ones), only a residual re-threads, and no
    sub-MT grandchild
    means every deep was absorbed — a stale thread is never an
    error; (ii) when one redistributed pair carries
    **two** deep signals (a range delete's two boundary survivors),
    healing the first may merge the second into a sibling — that second
    deep is then already healed by absorption, and its holder-scan miss
    is the legitimate outcome, not an error; when the 2-survivor cousin-cascade case (`leftIdx=0 ∧
    rightIdx=cellCount` at the parent of two below-MT survivors) leaves the
    parent degenerate, the still-below-MT page is threaded upward as
    `deepUnderflowChild` and rebalanced against its cousins after the
    parent's own cascade-merge produces a sibling-rich branch — see
    §Algorithm Phase 3 Rebalance below;
    **Parent-capacity decline.** A redistribute (leaf or branch pair)
    replaces the pair's boundary separator in the parent — recomputed
    via shortest-separator for leaves, lifted from the combined cell
    set for branches — and the replacement can be much longer than
    the separator it displaces (a byte-balanced boundary landing
    inside a long-shared-prefix cluster). A redistribute whose new
    separator the parent branch cannot **physically** encode (capacity
    is physical encoded size, the same bound as any branch encode)
    also **declines**: the check runs on the redistribute plan, and
    the redistribute allocates and frees nothing on decline. (For a
    leaf pair the preceding merge *attempt* allocates and frees one
    self-contained scratch page before the plan is reached; the
    decline itself changes no page state.) Merges never need this
    check: a merge removes a parent cell, which cannot grow the
    parent's encoding. A decline for parent capacity is handled
    exactly like a fill-floor decline — the underflowing child is
    threaded upward or accepted below-floor; the delete itself always
    succeeds. Without this clause a valid Delete/DeleteRange on such
    a topology would fail with an encode error after the sibling
    pages were already freed.
    (Enforced by `parentFits` in the merge/redistribute helpers;
    pinned by the parent-capacity-decline regression tests in
    `internal/btree/delete_test.go`.)
  from=this spec §Algorithm Phase 3 rebalance + `api-surface.md`
    Options.MergeThreshold godoc (the percentage doubles as the
    merge **trigger** AND the post-mutation **floor**) +
    `page-formats.md` §Branch Page (within-page truncation + the
    physical-vs-logical size distinction);
  violation=Two distinct failures. (1) **Reachable floor left
    unmet**: a `DeleteRange`/`Delete` whose cascade leaves a page below
    MT (logical) as a non-root child *when a feasible merge or
    logical-floor-clearing redistribute existed* — e.g. a redistribute
    balanced on COMPRESSED size that piled cheap same-cluster cells on
    one half and stranded the other below MT. The page-fill property the
    rest of the engine (compaction pacing, leak-detection
    page-utilization heuristics, splitting fairness) reasons against then
    drifts silently. (2) **Non-termination on an unreachable floor**:
    the rebalance loops/recurses trying to heal a page the floor cannot
    reach (a redistribute that merely relocates the sub-MT deficit to a
    sibling, which the cousin cascade then chases) — a valid in-spec
    `Delete` never returns / exhausts memory. The accepted below-floor
    page in the genuinely-unreachable regime is **not** a violation; it
    is this qualifier's sanctioned outcome.

## Algorithm (un-indexed keyspaces)

Three phases.

### Phase 1 — Find boundary paths

Descend the B+tree twice to find the left and right boundary paths.
A path is a stack of `(pageID, index)` pairs from root to leaf.

### Phase 2 — Identify and retire interior subtrees

Walk up from the two boundary paths to find their lowest common
ancestor (LCA). At each level between LCA and leaves:

- **Interior children** (between left and right boundaries) lie
  entirely within the range — their entire subtrees are retired
  without visiting individual leaves.
- **Boundary children** are partially within the range and must be
  descended.

Retiring a subtree walks the branch pages recursively. For each page
encountered, add its page ID to `tx.retiredPages`. For leaf pages,
accumulate the entry count for the return value. For overflow pages
referenced by leaf cells, retire the entire overflow run. The walk
visits every page in the subtree exactly once — `O(pages in
subtree)`.

### Phase 3 — Clean up boundary leaves and rebalance

- In the left boundary leaf: delete entries from `start` through end
  of leaf.
- In the right boundary leaf: delete entries from start through last
  key before `end`.
- If both boundaries are in the same leaf, delete entries between
  them.
- Retire any overflow pages referenced by deleted entries.
- Walk up from boundary leaves to LCA, removing the retired interior
  child pointers from each branch (CoW each branch).
- Rebalance: check fill ratios on modified branches and leaves. Merge
  or redistribute per `MergeThreshold` (see Options in
  `api-surface.md`).
- **Post-merge re-rebalance.** A single merge of two below-MT
  siblings can leave the merged page itself below MT. The local
  rebalance loop checks the merge result's fill against
  `MergeThreshold` — **logical** content for branches, physical fill
  for leaves (see the §Invariants two-size-notions) — and, when the
  result is still below the floor, re-attempts merge with the next
  adjacent survivor in the same parent. The loop converges in
  `O(survivors)` iterations.
- **Cousin-cascade rebalance.** When the recursed-into branch's
  survivor list reduces to a single still-below-MT child (the
  `leftIdx=0 ∧ rightIdx=cellCount` boundary case where both
  surviving children come back below MT and no third survivor
  remains at that level), the branch is degenerate. The
  still-below-MT child is threaded upward as a `deepUnderflowChild`
  signal alongside the `(newID, count, underflow)` return tuple. At
  the level above, after the local cascade-merge produces a
  sibling-rich branch that now holds the deep child as a direct
  child, the local code finds that child's position in the new
  branch and runs the same local-rebalance loop against its
  cousins. The loop terminates when either the deep child reaches
  the floor or the level above ALSO collapses to a single child —
  in which case the signal threads one more level up. Termination:
  bounded by tree depth.
- **Semantic-underflow rule for deepUnderflowChild propagation.**
  A branch returning `deepUnderflowChild != 0` is reported as
  underflow=true regardless of its encoded fill. Rationale: the
  level above runs the cousin pass only on the case-C (merge) path
  — case-B (child healthy, no merge) would thread the deep signal
  upward without giving the deep new siblings, and the cascade
  would reach the top with the deep buried unhealed. Tagging the
  branch as semantically-underflow forces the level above into
  case-C, where mergeOrRedistribute*-then-cousinRebalanceBranch
  exposes the deep as a child of a sibling-rich merge result that
  the cousin pass can heal.
- **Top-level final-heal pass.** After the top-level recursion
  returns `(newRootID, …, deepUnderflowChild)`, if the
  deepUnderflowChild is non-zero AND newRootID is non-zero, run
  one cousin pass at newRootID. The root collapse loop that
  follows promotes any residual to root (where it becomes exempt
  from the floor). Without this final pass, a cascade that
  exhausted every intermediate level's siblings would leave the
  deep as a sub-MT direct child of the new root — a non-root
  fill-floor violation.

This §Phase 3 covers both `Delete` and `DeleteRange` post-mutation
rebalance — the merge/redistribute primitives are shared, and the
fill-floor invariant applies to both call sites.

**Root collapse.** If rebalance reduces the keyspace's root to a
single child (a branch with one child pointer and no separators),
retire the root and promote the surviving child to the new root —
update the keyspace descriptor's `Root` field. If `DeleteRange`
emptied the keyspace entirely, retire the root and set `Root = 0`
(empty keyspace). The descriptor update is part of the same write
transaction and propagates up through the keyspace B+tree via CoW.

## Complexity

| Operation | Naive (cursor loop) | Range delete |
|-----------|---------------------|--------------|
| Delete N keys spanning P pages | O(N × depth) | O(P + depth²) |
| CoW'd pages | O(N × depth) | O(depth²) |
| Retired pages | N leaf cells + splits | P pages (bulk) + boundary cleanup |

Worked example: for 1M keys across 10K leaves at depth 4, a naive
loop CoWs ~4M pages; range delete walks ~10K pages and CoWs ~16 on
boundary paths.

## Set Keyspace Bulk Free

Deleting a key in a SetKeyspace whose values are in a nested B+tree
frees the nested tree via the same subtree retirement: read root +
count from the cell, walk the nested tree recursively retiring every
page, remove the cell. `O(pages in nested tree)`, not `O(values)`.
See `set-keyspace.md §Bulk Free`.

If the SetKeyspace has indexes declared, this falls back to a
per-member walk — same reasoning as `DeleteRange` on indexed
keyspaces.

## Set Keyspace Range Delete

`SetKeyspace.DeleteRange(start, end)` dispatches by index
presence, mirroring `Keyspace.DeleteRange`'s indexed/un-indexed split:

- **Un-indexed (Kind=1, no declared indexes)**: dispatches to
  `btree.DeleteRange` (the §Algorithm three-phase walker) with a
  SetKeyspace-aware per-cell free callback. Interior subtrees are
  retired by the walker's Phase 2 via `FreeSubtree` — the existing
  cell-type-aware retire that handles subpage (counts
  `subpage.Count`), nested-tree (recurses + counts `NestedCount`),
  and plain/overflow (counts 1) cells. Boundary leaves' Phase 3
  cleanup invokes the same callback per deleted entry, retiring
  the per-cell resources and contributing the per-cell values
  count. Returned count is total VALUES deleted (sum across
  subpages + nested-tree NestedCounts + plain cells) per the
  `set-keyspace.md` `desc.Count` entailed E2 accounting.
  **Atomic on error**: returns `(0, err)` with no observable
  mutations — `desc.Root`, `desc.Count`, and the
  dirty/cursor-stale state are touched only on the success return,
  so successive same-tx reads after a failed call observe the
  pre-call state; tx-level `Rollback` restores the on-disk pager
  bitmap to the pre-call state per `pager-slab.md`. Same
  all-or-nothing contract as `Keyspace.DeleteRange`. Cost:
  O(P + depth²) per §Complexity.

- **Indexed (Kind=1 with declared indexes)**: per-row cursor walk
  + `SetKeyspace.Delete(k)`, where each call invokes the
  `applyIndexMaintenanceOnBulkKeyDelete` to clear index entries
  per (setKey, setValue) pair via the extractor. Cost is
  `O(K × M × (indexes + extractor))` where K = keys in range, M =
  average set size per key. **Per-row atomic on error**: same
  partial-progress contract as the §Indexed-keyspace fallback
  below — returns `(deleted_so_far, err)` with the first i-1
  successful per-row deletes committed in-memory.

Both paths use the §Set Keyspace Bulk Free mechanism for
nested-tree cells (the un-indexed path via `FreeSubtree` at both
interior subtrees and boundary leaves; the indexed path via
`SetKeyspace.Delete`'s nested-tree branch per call).

## Indexed-keyspace fallback

`DeleteRange` on an indexed keyspace does NOT use the O(pages)
subtree-retirement fast path. The engine cannot retire a subtree
without knowing the prior-index-keys for every row in it (the
extractor output depends on the row's value, which the subtree-
retirement walk does not visit).

Implementation: the engine iterates the range with a cursor, calling
`Delete()` for each row. Cost is `O(entries × (indexes +
extractor))`. The cursor must remain stable across the CoW +
rebalance triggered by per-row deletes — `Cursor.Delete()` advances
the cursor to the post-delete successor in-place, so the canonical
drain pattern reads it via `Cursor.Current()` (NOT `Cursor.Next()`,
which would skip the successor). See `transactions.md §Cursor State
Machine` for the full pattern, and `§Cursor-Based Range Delete`
below for the worked example.

This is the same cost a SQL engine pays for `DELETE … WHERE … IN
range` with secondary indexes. Predictable and correct.

**Partial-progress on error (spec amendment).** Unlike
non-indexed `Keyspace.DeleteRange` — whose underlying
`btree.DeleteRange` is atomic and returns `(0, err)` with no
visible state change on failure — the indexed-keyspace cursor-
walk is per-row atomic, not per-call atomic. On a per-row failure
at iteration `i`, iterations `0..i-1` have already completed: each
of those rows is removed from the parent keyspace AND each of
their index entries has been cleared via the atomic
`Cursor.Delete` maintenance. `DeleteRange` returns
`(deleted_so_far, err)` so the caller sees the real scope of
state change. Each successful per-row delete satisfies the
keyed-removal invariants and the atomic-Put/
Delete invariant individually; the in-memory + on-disk state is
consistent-but-partial. The only safe recovery is `Tx.Rollback()`
(which restores via the pager bitmap snapshot per
`pager-slab.md`). The same per-row partial-progress contract
applies to the indexed `SetKeyspace.DeleteRange` dispatch — see
§Set Keyspace Range Delete above. The un-indexed
`SetKeyspace.DeleteRange` dispatch, like un-indexed
`Keyspace.DeleteRange`, is atomic via `btree.DeleteRange`.

Callers needing the O(pages) fast path on indexed data can:

- Drop the indexes before the bulk operation, run `DeleteRange`,
  then rebuild the indexes (`tx.Indexes().Rebuild`).
- Or use `DeleteKeyspace` to drop the whole keyspace (which also
  drops its indexes — the engine cleans up internal index keyspaces
  and the per-keyspace index registry).

## Cursor-Based Range Delete

For callers needing finer control:

```go
c := ks.Cursor()
for k, _ := c.SeekGE(start); k != nil && bytes.Compare(k, end) < 0; k, _ = c.Current() {
    c.Delete()
}
```

Note the use of `c.Current()` (not `c.Next()`) inside the loop:
`Cursor.Delete()` already advances the cursor to the post-delete
successor per `transactions.md §Cursor.Delete() post-delete state`,
so `Current()` reads it directly. `Next()` would step PAST the
successor and skip alternating entries. (Spec
amendment — earlier revisions of this example used `Next()`.)

One-at-a-time path. `DeleteRange` should be preferred for contiguous
unconditional deletes.

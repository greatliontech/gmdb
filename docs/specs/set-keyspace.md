# Set Keyspace Storage

A SetKeyspace maps each key to a **sorted set of values**. This spec
covers the on-disk storage strategies (subpage and nested B+tree),
the promotion/demotion thresholds between them, fixed-size value
sets, and the SetKeyspace-specific encoding for secondary-index
primary keys.

Scope:
- Subpage format (variable and fixed-size).
- Nested B+tree reference cells.
- Promotion / demotion rules.
- Indexes-on-SetKeyspaces PK encoding.

The user-facing API split between `Keyspace` and `SetKeyspace` lives
in `keyspaces.md` and `api-surface.md`. Range delete on un-indexed
SetKeyspaces dispatches to the same three-phase walker as
`Keyspace.DeleteRange` (per `range-delete.md §Algorithm`) — interior
subtrees are retired via `FreeSubtree` (which handles SetKeyspace
cell types via the recursive `freeSubtreeAt` per the §Bulk Free
mechanism below), and boundary leaves invoke a per-cell free
callback that runs the same bulk-free per nested-tree cell. Range
delete on INDEXED SetKeyspaces falls back to the per-row cursor walk
(`range-delete.md §Indexed-keyspace fallback`), same shape as
`Keyspace.DeleteRange`'s indexed dispatch. Indexing semantics for
SetKeyspaces live in `indexing.md`.

SetKeyspaces are a general-purpose data primitive for set-shaped
data: graph adjacency lists, inverted-index postings lists,
many-to-many relationships, pub/sub subscription registries,
Redis-ZSET-shaped storage (score-prefixed members), and audit logs
per entity. **SetKeyspace is not the secondary-index mechanism** —
secondary indexes use the dedicated indexing subsystem
(`indexing.md`).

## Invariants

Invariant: kind=clause-explicit;
  property=Empty value sets do not exist on disk, not even
    transiently within a write transaction. The last `DeleteValue`
    for a key also removes the key cell from its parent leaf;
  from=this spec §Demotion;
  violation=A "valued" key with zero stored values yields no rows on
    iteration but `Has(key)` returns true — the user-visible
    membership model breaks (set with no elements is observationally
    indistinguishable from a non-existent key, yet the engine
    reports it as existing).

Invariant: kind=clause-explicit;
  property=A subpage stores values in sorted (lexicographic) order;
    a nested B+tree's keys are the set values in sorted order with
    empty (zero-length) values;
  from=this spec §Storage Strategy + §Subpage Format;
  violation=An unsorted subpage breaks `HasValue` binary search
    (membership false-negative); duplicate or wrong-order entries in
    a nested B+tree cause range and set-cursor iteration to skip or
    repeat values.

Invariant: kind=clause-explicit;
  property=When a SetKeyspace is created with non-zero
    `FixedValueSize`, every value stored in any subpage or nested
    B+tree leaf for that keyspace is exactly that many bytes — and
    no subpage entry or nested leaf cell carries a per-value
    `ValueLen` prefix;
  from=this spec §Fixed-Size Value Sets;
  violation=A variable-size value smuggled into a fixed-size
    SetKeyspace decodes garbage entries (the binary-search direct
    offset arithmetic assumes uniform stride).

Invariant: kind=clause-explicit;
  property=Subpage promotion to a nested B+tree fires when inserting
    a new value would cause the subpage to exceed 50% of the leaf
    page's usable space (PageSize minus header, restart metadata,
    restart table, and optional checksum footer);
  from=this spec §Subpage Promotion Threshold;
  violation=Crossing the threshold without promoting overflows the
    parent leaf and corrupts the surrounding restart-group encoding;
    promoting too aggressively wastes a full page per small set and
    blows out leaf density.

Invariant: kind=clause-explicit;
  property=A SetKeyspace key whose nested B+tree shrinks to a single
    leaf page that would fit as a subpage is demoted back to a
    subpage in the same operation that deleted the precipitating
    value; the nested root leaf is freed;
  from=this spec §Demotion;
  violation=Persistent over-promotion leaves orphan one-leaf nested
    trees consuming a full page each, defeating the density goal of
    the subpage strategy.

Invariant: kind=clause-explicit;
  property=The SetKeyspace compound-PK separator `0x00 0x01` is
    lex-distinct from the NUL-escape terminator `0x00 0x00` and the
    escape sequence `0x00 0xFF`, and never appears inside an escaped
    column (the only `0x00` bytes in an escaped column are followed
    by `0xFF`);
  from=this spec §Indexes on SetKeyspaces;
  violation=A PK encoding that collides with a column terminator or
    escape produces an index key that decodes ambiguously to two
    different (column-tuple, PK) pairs — non-unique indexes can
    return wrong primary keys at lookup, and the index registry's
    fingerprint guarantees do not catch it.

Invariant: kind=entailed;
  property=The nested-tree reference cell's `Count` field equals the
    number of leaf entries reachable from `Root` for that key; every
    `Put` / `DeleteValue` that mutates the nested tree maintains the
    equality before returning to the caller;
  from=entailed: §Nested B+tree Reference Cell describes `Count` as
    "number of values in the set (O(1) access)" — but no single
    clause states the per-op accounting that maintains equality
    across promotion, in-place insert, in-place delete, and demotion;
  violation=A `Put` or `DeleteValue` that mutates the nested tree
    but skips the `Count` update returns wrong `CountValues(key)` —
    the user reads "37 members" while iterating 42 of them, or vice
    versa, with no clause stating they must match.

Invariant: kind=entailed;
  property=`desc.Count` for a SetKeyspace equals the sum over all
    keys `k` of values stored under `k` (subpage-stored or
    nested-tree-stored), maintained atomically with every Put /
    Delete / DeleteValue / DeleteRange / bulk-free;
  from=entailed: `keyspaces.md §Keyspace Descriptor` Count field
    "For a SetKeyspace, total pairs across all value sets" — but no
    single clause states the per-op accounting that maintains this
    equality across subpage growth, promotion (a single Put can
    increase Count by 1 and move N old members from subpage to
    nested tree), per-key bulk-free (a single Delete can decrement
    Count by N for the freed nested tree), and DeleteRange;
  violation=`ks.Stats().Entries` diverges from the actual stored
    pair count; iteration counts and `Stats()` disagree on the same
    transaction snapshot, breaking audit, capacity planning, and the
    `chunk-5.7 DeleteRange` defense-in-depth check that compares
    returned-count vs desc.Count.

Invariant: kind=entailed;
  property=Promotion (subpage → nested tree) and demotion (nested
    tree → subpage) are atomic within a single `SetKeyspace.Put` /
    `DeleteValue` / `Cursor.Delete` / `DeleteRange` call —
    observable only post-call; never mid-mutation. A failure inside
    the multi-step sequence leaves the SetKeyspace observationally
    unchanged within the same write transaction (any allocated
    pages are retired into loose/retired pools, cell content is
    restored); a subsequent `Has` / `HasValue` / `CountValues` /
    cursor op in the same tx after the failed mutation must
    observe the pre-call state. (Non-empty intermediate states
    DURING the call sequence — e.g., between step 2's leaf
    population and step 4's insert-new-value — are permitted; the
    "no empty sets" clause-explicit invariant above is unaffected
    because intermediate states carry the original `N` values,
    never zero.);
  from=entailed: §Subpage Promotion Threshold describes a 4-step
    sequence (alloc leaf, copy entries, replace cell, insert new
    value), and §Demotion describes the reverse — neither states
    that an error mid-sequence must leave the SetKeyspace
    observationally unchanged at the next same-tx read;
  violation=A failure after step 3 of promotion leaves the parent
    cell pointing at an allocated-but-unpopulated leaf —
    subsequent same-tx `HasValue` reads decode garbage from an
    empty leaf; the corresponding demotion failure mode leaves
    the cell carrying valid subpage bytes while still flagged as
    a nested-tree reference, with the nested root already freed —
    every same-tx read decodes the freed page's stale content as
    values.

Invariant: kind=entailed;
  property=`SetCursor.NextValue` from the last value in a key's set
    transitions the cursor to "value-EOF for this key" (next
    `NextValue` returns `nil`); only `Next` / `NextKey` advance
    across keys. Symmetric for `PrevValue` / value-BOF / `Prev` /
    `PrevKey`;
  from=entailed: `api-surface.md §SetCursor` declares `NextValue`
    and `NextKey` as separate methods, and `keyspaces.md
    §Iteration Semantics` documents `Next` to "advance through
    values within a key's set before moving to the next key" —
    without the value-bounded `NextValue` contract, the two
    methods (`Next` and `NextValue`) collapse to identical
    behavior and `NextKey` becomes the only way to bound an
    intra-key value iteration;
  violation=`NextValue` silently crosses key boundaries — a caller
    iterating one key's values via `NextValue` accidentally
    consumes the next key's set with no signal, mis-routing pub/sub
    deliveries to wrong subscribers, double-counting in
    ref-counted indexes, or merging audit-log entries across
    unrelated entities.

Invariant: kind=entailed;
  property=`SetKeyspace.DeleteRange` honors the atomicity-contract
    differential `range-delete.md §Set Keyspace Range Delete`
    declares: un-indexed call paths return `(0, err)` on failure
    with NO observable in-memory mutation (atomic; matches the
    `Keyspace.DeleteRange` un-indexed contract documented in
    `api-surface.md`); indexed call paths return
    `(deleted_so_far, err)` with iterations `0..i-1` committed in-
    memory (per-row atomic). The two contracts are user-facing,
    not implementation-internal — callers reason about post-error
    state via the documented contract;
  from=entailed: `range-delete.md §Set Keyspace Range Delete`
    declares the dispatch direction by index presence (clause-
    explicit at the algorithm level), and `api-surface.md
    SetKeyspace.DeleteRange` godoc names the resulting atomicity
    contracts — but no single clause states the dispatch is the
    SOLE mechanism producing the named contracts, so an impl that
    routes by index presence but produces the wrong contract on
    that path (e.g., un-indexed routed through a per-row helper
    that returns `(deleted_so_far, err)`) would honor the
    dispatch-direction declaration while silently violating the
    contract differential;
  violation=A caller invokes `SetKeyspace.DeleteRange(start, end)`
    on an un-indexed Kind=1 keyspace; the operation hits a pager
    error mid-walk (e.g., `ErrTxTooLarge` after `n` rows have
    been mutated). The api-surface.md godoc promises `(0, err)`
    with no observable mutation. The caller's subsequent same-tx
    read sees `n` rows already gone — partial mutation contradicts
    the documented atomic contract; the caller's recovery
    assumption ("I can retry the whole call after re-checking
    state") is silently wrong, producing a double-delete or an
    inconsistent index-vs-row state if the caller proceeds.
    Pinned by
    `TestSetKeyspaceDeleteRangeUnindexedDispatchesToWalker` /
    `TestSetKeyspaceDeleteRangeIndexedDoesNotDispatchToWalker` via
    the `SetDeleteRangeCalledHookForTest` instrumentation hook in
    `internal/btree/range_delete.go` — the dispatch direction is
    the necessary pre-condition for the contract differential to
    hold; testing the dispatch is the strongest enforcement
    available without fault-injection infrastructure.

## Storage Strategy

Two storage strategies based on value-set size:

- **Subpage (small value sets).** Values fit within the leaf cell,
  stored inline as a mini sorted list. No extra page allocation.
- **Nested B+tree (large value sets).** Promoted to a full B+tree
  whose root page ID is stored in the leaf cell. Each value becomes
  a key in the nested B+tree (with empty values).

## Subpage Format

A subpage is stored in the leaf entry's value area.
`CellFlags.MultiValue` is set and `CellFlags.NestedTree` is clear.
The entry uses the standard restart/delta key encoding from
`page-formats.md`; the subpage occupies the inline value half —
the `ValueLen u32` prefix from `page-formats.md §Leaf Page` is
present and equals `4 + DataSize` (the subpage header + entry
bytes). The leaf layer carries the subpage bytes opaque-through;
the subpage's internal structure (Count / DataSize / entries) is
decoded by the SetKeyspace layer, not by the leaf.

```
SetKeyspace Subpage Entry (restart)        Subpage size = ValueLen = 4 + DataSize
+-----------+----------+----------+-----------+-----------+
| CellFlags | KeyLen   | ValueLen | Key bytes | Subpage   │
| uint8     | uint16   | uint32   |           |           │
+-----------+----------+----------+-----------+-----------+
                                                          │
Subpage (embedded in cell value area) ←───────────────────┘
+----------+----------+---------+---------+-----+
| Count    | DataSize | Entry 0 | Entry 1 | ... |
| uint16   | uint16   |         |         |     |
+----------+----------+---------+---------+-----+
```

The delta-entry form mirrors `page-formats.md §Leaf Page
(Compressed Leaf) Delta entry` exactly:
`[CellFlags][SharedLen][UnsharedLen][ValueLen u32][UnsharedKey][Subpage]`.
Only the restart form is shown above; the delta form's `ValueLen`
field has the same semantics.

For **variable-size values**:

```
Entry (variable):
+----------+-----------+
| ValueLen | Val bytes |
| uint16   |           |
+----------+-----------+
```

For **fixed-size values** (keyspace declared with `FixedValueSize`):

```
Entry (fixed):
+-----------+
| Val bytes |  (size = keyspace's fixed value size, no length prefix)
+-----------+
```

`Count` is the number of entries. `DataSize` is the total byte size
of all entries.

Values within the subpage are stored in sorted (lexicographic) order.

**Lookup.** Fixed-size subpages: `O(log N)` binary search via direct
offset arithmetic (uniform stride). Variable-size subpages: `O(N)`
linear scan — subpages are bounded by the 50% promotion threshold
(N typically ≤ 200 for 4 KB pages at 8-byte values, fewer at larger
sizes), and the absence of a per-entry offset table preserves
subpage density: each offset would cost 2 bytes per entry, i.e.
`2/(value_body + 4)` overhead per entry — ~17% at 8-byte values,
~10% at 16-byte values, ~3% at 64-byte values — lowering the
effective promotion threshold across the small/medium-value range
and forcing earlier allocation of a 4 KB nested-tree leaf for sets
that would otherwise fit inline. The on-disk subpage format carries
no offset table; a future `SubpageReader` may build an in-memory
offset cache transparently if profiling demonstrates lookup is hot
— this is a non-format-breaking optimization.

Subpage entries are **not prefix-compressed**. Subpages store
*values* for a single key, which typically do not share prefixes by
construction (e.g., post IDs in a postings list). The subpage is
also small by definition (below the 50% promotion threshold).

## Subpage Promotion Threshold

A subpage is promoted to a nested B+tree when inserting a new value
would cause the subpage to exceed **50% of the leaf page's usable
space** (PageSize minus header, restart metadata, restart table, and
optional checksum footer).

Promotion:

1. Allocate a new leaf page for the nested B+tree.
2. Copy all subpage entries into the new leaf page as regular cells
   (where "keys" are the values from the set and "values" are
   empty).
3. Replace the subpage cell with a nested B+tree reference cell.
4. Insert the new value into the nested B+tree.

## Nested B+tree Reference Cell

```
SetKeyspace Nested B+tree Entry (restart)
+-----------+----------+-----------+----------+----------+
| CellFlags | KeyLen   | Key bytes | Root     | Count    |
| uint8     | uint16   |           | uint64   | uint64   |
+-----------+----------+-----------+----------+----------+
```

- **Root**: page ID of the nested B+tree's root.
- **Count**: number of values in the set (O(1) access).

Depth is not persisted — derived by reading the root page on first
access. The nested B+tree uses the same B+tree implementation as the
main keyspace; its "keys" are the values from the set, all "values"
are empty (zero-length). Nested-tree leaves use prefix compression
like all other leaves.

## Demotion

When deletions reduce a nested B+tree to a single leaf page that
would fit as a subpage, the B+tree is demoted back to a subpage. The
leaf page is freed; entries are packed inline into the parent leaf
cell.

When the last value for a key is deleted, the key's cell is removed
from the parent leaf entirely — empty nested trees and empty
subpages never exist, not even transiently within a write
transaction.

## Bulk Free (Delete on a key with a nested tree)

Deleting a key whose values are in a nested B+tree frees the nested
tree via subtree retirement: read `Root` + `Count` from the cell,
walk the nested tree recursively retiring every page, remove the
cell. `O(pages in nested tree)`, not `O(values)`. See
`range-delete.md §Set Keyspace Bulk Free`.

If the SetKeyspace has secondary indexes declared, the engine cannot
use the bulk-free fast path — it must walk every `(key, value)` set
member and call the extractor to compute prior index entries for
deletion. See `indexing.md §Indexes on SetKeyspaces` and
`§Bulk Operations on Indexed Keyspaces`.

## Cursor Value Streaming

A `SetCursor` position on a nested-tree key streams members through
a lazy cursor over the nested tree: positioning costs O(tree depth)
memory and time regardless of set size, and `CountValues` reads the
cell's persisted `NestedCount` in O(1). Subpage keys materialize
their value slice — bounded by one page by construction. The value
state machine (value-BOF / value-EOF, `SeekValue` leaving the
position unchanged on a miss) is uniform across both storage modes.

Invariant: kind=clause-explicit;
  property=A SetCursor position never allocates memory proportional
    to the current key's member count when the key is stored as a
    nested tree;
  from=this spec §Cursor Value Streaming;
  violation=Positioning on a postings-list key with millions of
    members (the workload §Overview targets) copies every member
    into memory per position — an O(set) allocation spike on a
    read path advertised as streaming.
    (Enforced by the nested-mode value helpers in set_cursor.go;
    pinned by TestSetCursorNestedPositionIsStreamed.)

## Fixed-Size Value Sets

When a SetKeyspace is created with `FixedValueSize` (non-zero), all
values must be exactly that byte size. Enables:

- No per-value length prefix in subpages (flat array).
- Direct offset binary search (`entry[i]` at `i * valueSize`).
- Compact nested B+tree leaves (no `ValueLen` field per cell).

A `Put` with a value of the wrong size returns `ErrValueSizeMismatch`.

## Indexes on SetKeyspaces

A SetKeyspace can carry secondary indexes. The extractor signature
is the same `func(key, value []byte) []IndexEntry`, but it runs
**per (key, value) set member**, not per top-level key. The
"primary key" in non-unique index entries is the `(key, value)` pair
— neither alone identifies the set member.

### Compound-PK encoding

Because the column terminator `0x00 0x00` already delimits columns
in the index key, the PK's internal split between its `key` and
`value` halves uses a distinct separator `0x00 0x01`. The PK is
encoded as:

```
escape(key) || 0x00 0x01 || escape(value)
```

then appended to the index key after the trailing `0x00 0x00`
column terminator, followed by a final `0x00 0x00` to terminate the
PK portion. `0x00 0x01` is lex-safely distinguishable from the
column terminator (`0x00 0x00`) and any escaped byte sequence
(`0x00 0xFF`), and never appears inside an escaped column (the only
`0x00` bytes in an escaped column are immediately followed by
`0xFF`).

Full grammar for a non-unique SetKeyspace index key:

```
indexKey  := escapedCol (0x00 0x00 escapedCol)* 0x00 0x00 escapedPK 0x00 0x00
escapedPK := escape(setKey) 0x00 0x01 escape(setValue)
```

A decoder splits the index key on the first `0x00 0x00` after the
last column terminator, then splits the PK on `0x00 0x01` to recover
`(setKey, setValue)`.

### Cursor delete and bulk-key delete

`SetCursor.Delete()` deletes one set member; index updates affect
only that member's contribution. `SetKeyspace.Delete(key)` removes
all members; index updates run the extractor on each removed
`(key, value)` pair.

Bulk-free of a key's nested B+tree (via `Delete(key)`) reverts to a
per-member walk when the SetKeyspace has indexes — same reasoning as
`DeleteRange` on indexed keyspaces.

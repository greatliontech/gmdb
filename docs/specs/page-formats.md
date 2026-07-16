# Page Formats

On-disk formats for the per-page structures stored in data pages:
branch pages (internal B+tree nodes — two layout variants, plain
`TypeBranch` and segregated `TypeBranchSegregated`), leaf pages
(three variants — interleaved prefix-compressed `TypeLeaf`,
segregated prefix-compressed `TypeLeafSegregated`, and uncompressed
`TypeLeafUncompressed`), and overflow pages (large value and key
storage). Set-keyspace subpages and nested B+tree references are
leaf-cell variants defined in `set-keyspace.md`. RPL segment pages
live in `free-space.md`.

Every page's first byte identifies its type AND layout variant;
readers dispatch on it and reject unknown values. Layout variants
are per-keyspace declared (`keyspaces.md §Per-Keyspace
Configuration`) but per-page dispatched: a reader NEVER derives a
page's layout from keyspace configuration — the keyspace can hold
a mix of variants during a configuration transition, exactly as it
can hold mixed compressed/uncompressed leaves.

Scope:
- Branch page layouts (plain, segregated), separator computation.
- Interleaved and segregated compressed leaf layouts with
  variable-size restart groups, restart vs delta entries, overflow
  references.
- Uncompressed leaf page layout (selected when `RestartGroupTarget
  == 1`).
- Leaf lookup, insert/delete, split, and the `LeafIter`
  bidirectional iterator used by `btree.Cursor`.
- Overflow-key cells: the inline threshold `T`, leaf and branch
  overflow-key forms, comparison and lifecycle rules.
- Overflow page format, run construction, and the whole-run
  digest.
- NUL-escape encoding for composite index keys.

Depends on / interacts with:
- `file-layout.md` for the common page header.
- `checksums.md` for per-page XXH3-64 footers.
- `indexing.md` consumes the NUL-escape rules defined here.
- `limits.md` states the key encoding bound and tabulates the
  inline threshold `T` this spec derives from the branch-page
  budget.

## Invariants

Invariant: kind=clause-explicit;
  property=A branch separator `S` between left child `L` and right
    child `R` satisfies `max(L) < S <= min(R)` — every key in the
    left subtree compares strictly less than `S`, every key in the
    right subtree compares greater than or equal to `S`;
  from=this spec §Separator Computation (Cross-Level Truncation);
  violation=A separator outside this range routes a search to the
    wrong subtree (key with `S <= k < min(R)` falls left and is not
    found; key with `max(L) < k < S` falls right and is not found),
    so Get returns `ErrNotFound` for keys that are actually present —
    silent data loss to the caller.

Invariant: kind=entailed;
  property=A segregated-branch cell stores only the separator suffix
    after the page-wide prefix `P` (`PrefixLen` bytes); the full
    separator is `P || Suffix[i]`. A reader MUST account for `P`
    before comparing suffixes — compare `target` against `P` first
    (descend leftmost when `target < P`, rightmost when `target > P`
    without sharing all of `P`) and binary-search suffixes only for a
    `target` that starts with `P`. The encoding round-trips:
    `decode(encode(cells))` reconstructs every cell's full key
    `P || Suffix[i]` and child exactly. (The plain branch stores full
    separators — no prefix accounting exists to get wrong.);
  from=entailed: the clause-explicit separator-routing invariant above
    fixes the separator VALUES but not how a within-page-compressed
    branch stores or searches them; no single clause states that the
    page-prefix split must round-trip and that search must reconstruct
    it;
  violation=A target `t` with `t[:k] == P[:k]` diverging at some
    `k < PrefixLen` routed by a suffix-only search (one that skips the
    prefix comparison) descends the wrong child — every
    separator-routing clause still holds, yet `Get` returns
    `ErrNotFound` for a key that is present. Equivalently, a
    prefix/suffix split that drops or duplicates a boundary byte
    reconstructs a wrong full key on decode and on the split-time lift.

Invariant: kind=entailed;
  property=A reader dispatches a page's layout variant by the page's
    type byte alone, never by the owning keyspace's declared layout
    configuration;
  from=entailed: layout declarations are mutable builder hints
    (`keyspaces.md §Per-Keyspace Configuration`) and a keyspace holds
    mixed-variant pages during a transition — the `RestartGroupTarget`
    precedent; no clause states that the per-page type byte, not the
    descriptor, is the decode authority;
  violation=A layout config change mid-transition makes a
    config-driven reader decode an old-variant page with the new
    variant's offset arithmetic — fabricated entries or slice-bounds
    panics on a page every write-path rule produced correctly.
    (Enforced by TestKeyspaceLayoutDeclaration — mixed-variant
    pages stay readable across declaration flips and reopen.)

Invariant: kind=entailed;
  property=Every supported BRANCH layout holds at least two
    worst-case overflow-key cells (`PrefixLen == 0` where the layout
    has a prefix) at every page size, and every supported LEAF
    layout holds at least ONE maximal-form entry (overflow key +
    overflow value) per page — the split-feasibility floor is
    per-layout, preserved by construction under the shared inline
    threshold `T`. The floors differ by node kind because splits
    do: a branch split must leave two routable separators per half,
    while a leaf split may legally produce single-entry leaves;
  from=entailed: `limits.md §Maximum Key Size` derives `T` so a
    branch can always split; making layouts per-keyspace silently
    re-scopes that floor — the spec is incoherent if some declared
    layout cannot hold two separators, or if a legal entry cannot
    be encoded on an empty page of its own layout;
  violation=A keyspace declared to a layout whose per-cell overhead
    exceeds the derivation's budget reaches a branch page that must
    split but cannot encode two overflow separators — the insert
    wedges with no in-spec recovery path (every other clause holds:
    the keys are legal, the cut is at `T`, the extents are valid).
    Two maximal leaf entries per page is NOT required and does not
    hold (two overflow-key + overflow-value entries exceed
    `PageSize` at every size); requiring it would misderive `T`.
    (Leaf floors enforced by TestLeafFloorOneMaximalEntryEveryLayout;
    per-variant branch floors by TestInlineThresholdValues — both
    layouts' two-cell budgets at every page size and checksum mode —
    and by TestBranchOverflowCellRoundTrip's two-cell floor fixtures,
    which encode, validate, and tight-side-reject under each layout.)

Invariant: kind=clause-explicit;
  property=Within a leaf restart group, the entry at the group's
    restart position stores a full key; every subsequent delta entry
    in the group encodes its key as `previous[0:SharedLen] ||
    UnsharedKey`;
  from=this spec §Leaf Page (Restart entry / Delta entry);
  violation=A delta entry whose `SharedLen` does not actually share
    `SharedLen` bytes with the previous entry's full key
    reconstructs a wrong key — cursor reads return wrong keys and
    binary search lands in the wrong group. The structurally
    checkable half — `SharedLen` never exceeds the previous entry's
    full-key length — is enforced at the read trust boundary
    (`LeafReader.Validate`): an unbounded `SharedLen` makes decode
    either panic or fabricate key bytes from adjacent page content.
    (Enforced by `TestLeafReader_Validate_TotalOverInput` SharedLen
    cases in `leaf_test.go`.)

Invariant: kind=clause-explicit;
  property=A leaf page's entry data is one contiguous stream
    starting at the variant's entry-data start (byte offset 12;
    segregated: 14) and ending exactly at `DataEnd`, entries in
    index order; every lookup-table entry (compressed restart-table
    Offset, uncompressed offset-table slot) equals its entry's
    position in that stream. In the segregated variant the stream
    holds entry headers and key bytes only — value bytes live in
    the value region and are excluded from this clause;
  from=this spec §Cursor Iteration and §Leaf Page — the consumers
    that decode by CONTINUATION with the unchecked hot-path
    decoders: the compressed streaming iterator and first-key
    reads, the compressed splice paths' continuation walks (which
    resume decoding mid-page from a restart point), and the write
    side's placement of the next entry at `DataEnd`. (The
    uncompressed iterator is table-driven on every step per
    §Cursor Iteration's O(1)-via-table clause — for it the
    invariant protects the positional table's agreement with the
    stream, not a mid-stream continuation.);
  violation=A page whose table offsets each pass a range check but
    do not match the stream (garbage bytes at offset 12 with the
    table pointing past them; a gap or overlap between restart
    groups) passes a per-table-offset walk, then a streaming read
    decodes bytes validation never examined — a slice-bounds panic
    or fabricated entries on a checksums-off page. A `DataEnd` past
    the stream end (trailing slack) instead corrupts on WRITE: the
    splice paths validate and then append the next entry at
    `DataEnd`, placing it outside the stream the readers decode.
    (Enforced by `LeafReader.Validate` exact stream-position
    matching; pinned by `TestLeafReader_Validate_TotalOverInput`
    contiguity cases and `FuzzLeafValidateTotal` — Validate-accepted
    pages must survive a full Iter walk + SearchLeaf — in
    `leaf_test.go`.)

Invariant: kind=clause-explicit;
  property=A compressed leaf's `RestartCount × 4` bytes immediately
    before the optional 8-byte XXH3-64 footer constitute the
    restart table; each entry stores the group's first-entry byte
    offset (uint16), the group's entry count in `[1, 255]` (uint8),
    and a reserved byte; the restart table indexes only entries on
    this page;
  from=this spec §Leaf Page (Compressed Leaf — page layout);
  violation=Misplacing the restart table (relative to the optional
    checksum footer) corrupts the binary-search index — lookups
    diverge into delta entries treated as restart points and decode
    garbage keys. A `Count` of zero in any restart-table entry
    leaves the next group's start ambiguous (the variable-group
    format derives group ranges by summing counts), so readers
    must treat `Count == 0` as structural corruption.

Invariant: kind=clause-explicit;
  property=An uncompressed leaf's `Count × 2` bytes immediately
    before the optional 8-byte XXH3-64 footer constitute the
    offset table; the table is **positional** — slot `i` holds
    the byte offset of entry `i`'s first byte (`CellFlags`).
    Entries themselves are key-sorted, so iterating the table in
    slot order yields entries in key order, but the table is
    indexed by position, not by key;
  from=this spec §Leaf Page (Uncompressed Leaf — page layout);
  violation=An offset that points outside the entry-data region
    yields out-of-bounds reads in the UC decoder; swapping slots
    `i` and `j` for `i < j` with `entries[i].Key > entries[j].Key`
    silently violates the binary-search contract.

Invariant: kind=clause-explicit;
  property=An overflow run of `1 + N` physically consecutive pages
    stores `(PageSize - HeadMeta) + N * PageSize` extent bytes as ONE
    contiguous byte range starting at the head page's content start
    (`HeadMeta` = 16 with `PageChecksum`, 8 without). The head page
    carries `AdditionalPages = N` in its header and, when
    `PageChecksum` is enabled, an 8-byte XXH3-64 whole-run digest
    immediately after the header, computed over the run's FULL
    content range (content start through the last follower's end),
    with slack bytes past the extent length zero on write; follower
    pages carry no header, no footer, and no digest — nothing
    interrupts the extent byte range;
  from=this spec §Overflow Page;
  violation=Reading a value with the wrong run-length truncates the
    value (short read) or runs past the run into another page
    (returning interleaved bytes from an unrelated allocation). Any
    per-page metadata inside the run (the retired follower-footer
    form) punches holes in the range — a borrowed single-slice value
    would expose digest bytes as value data. A digest over only the
    extent length is unverifiable from the head alone (the length
    lives in the referencing cell), so the proactive scrub either
    skips runs or false-positives on every run with slack.
    Enforced by `TestOverflowRunLengthBoundaries` and
    `TestOverflowRunDigestCoversFullContentRange` (`internal/page`)
    and `TestOverflowRunPagesCarryNoFooters` (package `gmdb`).

Invariant: kind=clause-explicit;
  property=A segregated-leaf entry locates its value by an absolute
    page offset (`VOff`); the value's LENGTH is derived by entry
    order — the next entry's `VOff` in entry order, or `ValueEnd`
    for the last entry — never by comparing value addresses;
  from=this spec §Segregated Leaf;
  violation=Zero-length values make adjacent entries share a
    `VOff`; any maintenance rule keyed on value addresses ("shift
    every value at-or-above X") misclassifies the boundary entry —
    one splice later an entry's derived length swallows its
    neighbour's value bytes while every offset individually stays
    in range.
    (Enforced by TestSegLeafZeroLengthValueAliasing and
    TestSegValidateRejectsVOffRegression; splice maintenance fuzzed
    by FuzzSegTryInsertAt / FuzzSegTryDeleteAt in `internal/page`.)

Invariant: kind=clause-explicit;
  property=The NUL-escape encoding (every `0x00` inside a column →
    `0x00 0xFF`; column boundary → `0x00 0x00`) is prefix-free: no
    escaped column is a prefix of another, and the column terminator
    `0x00 0x00` never appears inside an escaped column;
  from=this spec §NUL-escape encoding;
  violation=A column whose escaped form is a prefix of another's
    breaks lex ordering of the concatenated tuple — index range
    queries return wrong matches, and a unique-index probe can
    accept a "duplicate" because the prefix tuple sorts adjacent.

Invariant: kind=entailed;
  property=At fixed column count, the encoder is tuple-prefix-free:
    for any two distinct tuples `T1`, `T2` with `len(T1) == len(T2)`,
    neither `encode(T1)` is a prefix of `encode(T2)` nor vice versa.
    (Different-column-count tuples CAN prefix-collide — a 2-col
    tuple's encoding is a prefix of a 3-col extension — which is by
    design: an index has a fixed schema, so the decoder always
    processes the same column count.);
  from=entailed: the clause-explicit column-level prefix-freeness
    above + the per-column `0x00 0x00` terminator together imply
    that two same-length tuples differing at column k diverge at
    column k's escaped bytes, with no terminator-induced prefix
    confusion; no single clause states the tuple-level property
    that index range queries actually depend on;
  violation=An index range query at a fixed schema returns a
    tuple whose encoded form prefix-matches a shorter-encoded
    same-arity tuple — the cursor mis-classifies adjacency,
    yielding wrong matches for `Range(start, end)` and false
    duplicates for unique-index probes. (Spec amendment;
    enforced by `TestEncodedTuplePrefixFreenessSameNColsProperty`
    in `index_key_codec_test.go`.)

Invariant: kind=clause-explicit;
  property=A key (leaf full key, or branch separator) longer than
    the inline threshold `T(PageSize, PageChecksum)` is stored as
    an overflow-key cell whose extent cut is FIXED at byte `T` of
    the full key: the extent holds `key[T:]`, and the resident
    inline bytes are `key[0:T]` in a leaf and `key[PrefixLen:T]`
    in a branch (reconstructible as `P || Inline`). A key `<= T`
    uses the pre-existing inline forms unchanged. `T` is a pure
    function of the page configuration (§Overflow-Key Cells);
  from=this spec §Overflow-Key Cells;
  violation=A cut that varies with page-local state (a suffix-
    relative cut) makes a separator move between pages with
    different prefixes require rewriting or re-slicing the extent
    — either copying it (double-free bookkeeping on retirement)
    or decoding a wrong key; a variable inline length breaks the
    §Leaf Split deterministic-encoding invariant — a `Check()`
    re-encode produces different bytes than the original writer.
    (Pinned by TestInlineThresholdValues, TestOverflowKeyLeafRoundTrip,
    TestBranchOverflowCellRoundTrip.)

Invariant: kind=clause-explicit;
  property=Comparison against an overflow-key cell is stated over
    FULL keys: the stored key's first `T` bytes are always
    resident (`key[0:T]` in a leaf; `P || Inline` in a branch),
    and the order is decided from them alone whenever the compared
    full key diverges within its first `T` bytes or has length
    `<= T` (a full match by a `<= T` key is then a strict prefix
    of the stored key — strictly less, no extent read). The extent
    is read exactly when the compared key's length exceeds `T`
    and `compared[0:T] == stored[0:T]`;
  from=this spec §Overflow-Key Cells (Comparison);
  violation=Deciding an order on a first-`T`-bytes tie without
    reading the extent routes a lookup to the wrong child or leaf
    position — `Get` returns `ErrNotFound` for a present key whose
    first `T` bytes collide with a neighbor's, and inserts land
    out of order (silent sort-order corruption). A branch rule
    stated over the page-relative portion instead of the full key
    is dimensionally wrong for `PrefixLen > 0` — a shared-prefix
    target shorter than `PrefixLen + T` can tie through the inline
    bytes while the true order lives in the extent.
    (Pinned by TestCompareEntryKeyExtentRule and
    TestBranchOverflowCellRoundTrip; end-to-end by
    TestPutStoresOverThresholdKey.)

Invariant: kind=clause-explicit;
  property=An overflow-key leaf entry is always a restart entry and
    always the sole entry of its restart group; a delta entry never
    carries an overflow key and never delta-encodes against an
    overflow-key predecessor. Consequently every delta-reconstructed
    full key is `<= T`;
  from=this spec §Overflow-Key Cells (Leaf forms);
  violation=A delta against an extent-resident predecessor forces
    extent reads inside the per-step `LeafIter` key reconstruction
    (unbounded `keyBuf` growth), and a reconstructed over-`T` key
    re-encoded by the delete keep-set rebuild cannot fit its cell —
    violating §Insert and Delete's rebuild-never-fails clause.
    (Pinned by TestOverflowKeyLeafRoundTrip's singleton-group check
    and TestValidateRejectsMalformedOverflowKey.)

Invariant: kind=entailed;
  property=A key extent follows the same lifecycle rules as a value
    overflow run: referenced by exactly one logical entry, carried
    by reference (never copied) across CoW page rewrites, splits,
    merges, and separator moves, and retired through the RPL when
    the referencing entry (or separator) is removed or its key
    replaced;
  from=entailed: §Overflow-Key Cells defines key extents as
    §Overflow Page runs, whose retirement clauses are written for
    values; no clause states that the key-extent reference is
    ownership-carrying across structural moves;
  violation=Copying the extent on a separator move double-frees the
    run when both referents retire (corrupting an unrelated later
    allocation); dropping the reference without RPL retirement
    leaks pages that `Check()` attributes to no keyspace.
    (Pinned by TestOverflowKeyLifecycleThroughSplitsAndDeletes's
    slab-partition checks, TestOverflowKeyWalkVisitsExtents, and
    TestOverflowKeyRelocation.)

## Branch Page (Internal B+tree Node)

Branch pages store separator keys and child page pointers. They do
NOT store values. Keys are stored in sorted order; for a branch
with N cells (N keys) there are N+1 child pointers: `Ptr[0]`
(leftmost, stored after the page header) plus one child pointer per
cell. The descent contract is layout-independent: the descent index
`i` is the number of separators `<= target` — `i == 0` → `Ptr[0]`;
`0 < i <= N` → the child pointer of cell `i-1`. When `target`
equals a separator, the search returns the index past it, so the
target descends into that separator's right child (separators are
right-child lower bounds; §Separator Computation).

Two branch layout variants exist, declared per-keyspace
(`keyspaces.md §Per-Keyspace Configuration`) and dispatched
per-page by the type byte:

- **Plain branch (`TypeBranch`).** Full separator bytes per cell.
  Every binary-search probe compares directly against stored
  separator bytes — no prefix gate, no reconstruction. The
  per-descend latency floor; a keyspace declares it when its
  trees are small and hot enough that per-level CPU outweighs
  density.
- **Segregated branch (`TypeBranchSegregated`, default).** The
  separators' single shared prefix stored once, suffix bytes
  packed in a heap addressed by an offsets-only directory, child
  pointers in a separate array. Denser wherever separator
  prefixes survive minimization (measured up to +42% fanout on
  MVCC-shaped keys) — the default favors fanout, which at scale
  buys tree depth, working-set size, and write amplification.

Both encodings are pure functions of `(cfg, leftmost, cells)` — a
`Check()` re-encode is byte-identical to the original writer's
output (the §Leaf Split deterministic-encoding invariant, for
branch pages).

### Plain Branch (`TypeBranch`)

```
Plain Branch Page
+-----------------------+ offset 0
| Page Header (8 bytes) | Type=TypeBranch, Count=N
+-----------------------+ offset 8
| Ptr[0] (uint64)       |  leftmost child pointer (8 bytes)
+-----------------------+ offset 16
| Cell Directory        |  Array of (Offset uint16, KeyLen uint16)
| ...                   |  grows forward, 4 bytes per cell
+-----------------------+
|       free space      |
+-----------------------+
| ...                   |
| Cell Data 1           |  each cell: KeyBytes || ChildPtr(uint64),
| Cell Data 0           |  packed backward from ContentEnd
+-----------------------+ ContentEnd (PageSize - optional 8-byte footer)
```

Each cell stores the FULL separator bytes followed by its right
child pointer; the directory's `Offset` points at the separator's
first byte and `KeyLen` gives its length. Search is a binary
search over the directory comparing `target` against the stored
bytes directly.

Bit 15 of the directory's `KeyLen` marks an **overflow branch
cell** (§Overflow-Key Cells): a cell whose full separator exceeds
`T` bytes. The low 15 bits give the inline length — always exactly
`T`, which fits 15 bits at every page size — and the cell data is
`Inline(T bytes) || KeyExtPage(uint64) || KeyTotalLen(uint32) ||
ChildPtr(uint64)`. The inline bytes are `separator[0:T]` and the
extent holds `separator[T:]`; comparison follows the
§Overflow-Key Cells rule. Cells whose full separator is `<= T`
are unchanged.

### Segregated Branch (`TypeBranchSegregated`)

```
Segregated Branch Page
+-----------------------+ offset 0
| Page Header (8 bytes) | Type=TypeBranchSegregated, Count=N
+-----------------------+ offset 8
| Ptr[0] (uint64)       |  leftmost child pointer (8 bytes)
+-----------------------+ offset 16
| PrefixLen (uint16)    |  length of the page-wide shared prefix P
| Reserved  (uint16)    |  zero on write (keeps the directory at offset 20)
+-----------------------+ offset 20
| Offsets Directory     |  (N+1) × uint16, heap-relative, growing
| ...                   |  forward; slot N is the heap-end sentinel
+-----------------------+ heap base = 20 + 2×(N+1)
| Suffix Heap           |  suffix bytes packed forward in key order
+-----------------------+
|       free space      |
+-----------------------+
| Child Pointer Array   |  N × uint64, packed ending at the prefix
+-----------------------+ ContentEnd - PrefixLen - 8×N
| Prefix bytes P        |  the page-wide shared prefix, PrefixLen bytes
+-----------------------+ ContentEnd (PageSize - optional 8-byte footer)
```

Cell `i`'s suffix occupies heap span `[Off[i], Off[i+1])` — the
directory stores offsets only; lengths derive from adjacent slots,
with slot `N` as the sentinel naming the heap's end. Offsets are
heap-relative, so growing the directory by one slot moves the heap
base without rewriting any stored offset. The full separator of a
(non-overflow) cell is `P || heap[Off[i]:Off[i+1]]`. Cell `i`'s
child pointer sits at `ContentEnd - PrefixLen - 8×(N - i)`.

`P` is the common prefix of the whole sorted separator set —
`commonPrefix(first, last)` — **capped at `T` bytes**
(`PrefixLen <= T`; the cap keeps `P` physically resident and part
of the deterministic pure-function encoding when separators share
a longer-than-`T` prefix; a shorter-than-true-common `P` is always
correct, the residue simply stays in the suffixes). At
`PrefixLen == 0` every cell stores its full key in the heap.

**Search algorithm.** Let `m = PrefixLen` and `P` the prefix bytes:

1. If `len(target) >= m` and `target[:m] == P`: binary-search
   `target[m:]` against the heap suffixes for the first suffix
   strictly greater than `target[m:]`. That index `i` is the
   descent index.
2. Otherwise `target` does not start with `P`: if `target < P`
   every separator exceeds it → descend leftmost (`i == 0`); if
   `target > P` every separator is below it → descend rightmost
   (`i == N`).

Comparing `target` against `P` once per page — rather than against
each full key at every binary-search probe — makes the descent
compare fewer bytes than a plain branch when separators share a
prefix.

An **overflow branch cell** is marked by bit 63 of its CHILD
POINTER. Bit 63 of every stored page ID is RESERVED by this
format: page IDs above `2^63 - 1` are structurally invalid
everywhere (the read path's reachability bound
`min(fileSize / PageSize, MaxSize)` — `checksums.md §Structural
and Allocation Bounds` — rejects them long before they could be
reached, and the `MaxSize`-sized mmap reservation keeps real IDs
astronomically lower), so the bit is never ambiguous. The
directory's offset field cannot carry the marker because heap-end
sentinels need the full uint16 range at 64 KB pages. A marked cell's heap span is
`Inline(T - PrefixLen bytes) || KeyExtPage(uint64) ||
KeyTotalLen(uint32)` — span length exactly `(T - PrefixLen) + 12`
— and the child pointer is the array word with bit 63 cleared.
The inline bytes are `separator[PrefixLen : T]` and the extent
holds `separator[T:]` — the extent cut is FIXED at byte `T` of the
full separator, independent of any page's `P`, so a separator move
between pages with different prefixes re-slices only the inline
bytes (fully reconstructible as `P || Inline` without touching the
extent) and carries the extent by reference unchanged. The suffix
binary search compares the inline portion first and reads the
extent only per the §Overflow-Key Cells comparison rule. A page
whose separators share a `>= T` common prefix caps `P` at `T`
(§ above), so an overflow cell's inline length is `>= 0` and the
`P || Inline` reconstruction always covers the first `T` bytes.

### Separator Computation (Cross-Level Truncation)

Branch pages store **cross-level-truncated separator keys** — the
shortest byte string that distinguishes the left subtree from the
right — rather than full keys copied from leaf pages, in every
branch layout. A branch separator satisfies:

- Every key in the left child compares **strictly less than** the
  separator.
- Every key in the right child compares **greater than or equal to**
  the separator.

Equivalently: `max(left) < separator <= min(right)`. The separator
is a lower bound of the right child.

**Separator computation** at leaf split: let `L` = the last key of
the left leaf, `R` = the first key of the right leaf. Compute the
shortest byte string `S` such that `L < S <= R` — the common prefix
of `L` and `R`, extended by one byte from `R` at the first
divergence position:
`S = R[0 : len(commonPrefix) + 1]`.
Insert `S` (not `R`) into the parent branch page.

At merge time, the separator is removed from the parent. At
redistribute time, the separator is recomputed from the new boundary
keys and the parent updated.

**Two complementary compressions.** Branch separators are compressed
*across* tree levels (the shortest distinguishing prefix, this
section) and — in the segregated branch layout — *within* a page
(§Segregated Branch: the one common prefix of a page's separators
stored once + per-cell suffixes). Leaf pages independently compress
redundancy within a page via restart-group delta encoding
(§Compressed Leaf). Cross-level and within-page truncation compose:
a leaf-adjacent segregated branch whose separators all share a long
cluster prefix stores that prefix once, so its fan-out stays high
even when each separator approaches the inline threshold `T`
(§Overflow-Key Cells) — the case that, in the plain layout,
collapses fan-out toward 2 (a branch holding only ~2 near-`T`
separators): a depth/breadth cost — such branches are byte-full and
sit ABOVE the `range-delete.md §Invariants` fill-floor by its own
logical-fill metric — but every descent pays the extra levels.
Keyspaces whose branch separators can approach `T` with shared
prefixes should declare the segregated branch layout.

**Interaction with key size**: separator truncation and within-page
prefix truncation are density optimizations — they cannot bound key
size, because the worst case is two separators sharing no prefix
(`PrefixLen == 0`), each needing full residence in one branch page
to split. That worst case is what bounds the INLINE threshold `T`
(§Overflow-Key Cells): every branch layout must hold two
overflow-key cells at zero shared prefix, and keys or suffixes past
`T` spill to key extents instead of constraining the format.
Separators are always `<=` the full keys they distinguish, and are
long only when the boundary keys share a long prefix — exactly the
case within-page truncation then compresses.

## Leaf Page

gmdb supports three leaf page variants chosen per-page at build
time by the keyspace's `RestartGroupTarget` and declared leaf
layout (`keyspaces.md §Per-Keyspace Configuration`):

- **Compressed, interleaved (`TypeLeaf`).** Keys that share common
  prefixes with their neighbours are stored as deltas grouped into
  **variable-size restart groups**; each entry's value bytes
  follow its key bytes in one stream; two-phase lookup (binary
  search over the restart table + linear scan within the matched
  group). Selected when `RestartGroupTarget ≥ 2` and the keyspace
  declares the interleaved leaf layout.
- **Compressed, segregated (`TypeLeafSegregated`, default).** The
  same restart-group key compression, but entry headers and key
  bytes pack contiguously from the page front while value bytes
  live in a separate region located by per-entry value offsets —
  the search region is pure headers-and-keys. Selected when
  `RestartGroupTarget ≥ 2` and the keyspace declares the
  segregated leaf layout.
- **Uncompressed (`TypeLeafUncompressed`).** Every key stored in
  full, interleaved; lookup is a single O(log N) binary search via
  an offset table. Selected when `RestartGroupTarget == 1`,
  regardless of declared leaf layout.

Group-size semantics are shared by both compressed variants: group
sizes are a TARGET, not a bound — in-place inserts may grow a
group up to `min(2 × RestartGroupTarget, 255)` before the splice
declines and a rebuild rebalances it — persisted, observable
on-disk state (a reader must accept groups anywhere in `[1, 255]`
regardless of the keyspace's current target; 255 is the hard uint8
count cap).

All variants share the 8-byte common page header and the
"entries-forward, lookup-table-backward" layout; only the
per-entry encoding, value placement, and lookup machinery differ.
The distinct type bytes let `Check()` and the readers dispatch
without probing further — never from keyspace configuration
(§Invariants).

Each entry carries a `CellFlags` byte (same definitions across all
variants):

```
CellFlags bit layout
Bit 0:    Overflow    (0 = inline value, 1 = overflow reference)
Bit 1:    MultiValue  (0 = single value, 1 = multi-value data — subpage or nested B+tree)
Bit 2:    NestedTree  (only when Bit 1 set: 0 = subpage, 1 = nested B+tree)
Bit 3:    EmptyValue  (compact inline form for an empty value: the
          ValueLen field and value bytes are absent entirely)
Bit 4:    OverflowKey (0 = inline key, 1 = key stored as inline
          threshold prefix + key-extent reference; see
          §Overflow-Key Cells)
Bits 5-7: Reserved (must be 0)
```

`Overflow` and `MultiValue` are mutually exclusive in practice.
`EmptyValue` is exclusive with the VALUE-form bits (0–2) — the
trailer and subpage forms carry their own value halves — and
composes freely with `OverflowKey`, which modifies only the key
half of the entry (§Overflow-Key Cells). In the INTERLEAVED and
UNCOMPRESSED variants, encoders emit `EmptyValue` for every plain
cell whose value is empty — nested B+tree members and the
set-of-keys pattern — saving the 4-byte `ValueLen` per cell;
decoders also accept the legacy `ValueLen == 0` inline form, so
mixed-form pages are valid. In the SEGREGATED variant the bit is
never set and is rejected at the read trust boundary — value
lengths are derived there, so an empty value is simply a
zero-length value-region span (§Segregated Leaf).

`ValueLen` is `uint32` (max ~4 GB for inline values; bounded in
practice by leaf-page free space). Values exceeding leaf-page
capacity are stored as overflow pages, referenced via the formats
below which use `uint64 TotalLen`.

### Compressed Leaf (`TypeLeaf`)

```
Compressed Leaf Page
+-----------------------+ offset 0
| Page Header (8 bytes) | Type=TypeLeaf, Count=N
+-----------------------+ offset 8
| RestartCount uint16   |  number of restart groups
| DataEnd      uint16   |  byte offset after the last entry's bytes
+-----------------------+ offset 12
| Entry 0 (restart)     |  entries in forward order starting at offset 12
| Entry 1 (delta)       |
| ...                   |
| Entry N-1             |
+-----------------------+ DataEnd
|       free space      |
+-----------------------+
| Restart Table         |  RestartCount × 4 bytes, packed at content end
+-----------------------+ ContentEnd (PageSize - optional 8-byte footer)
```

`RestartCount` and `DataEnd` are little-endian `uint16` (per the
project-wide byte-order rule in `overview.md`). `DataEnd` is the
byte offset where entry data ends; `[DataEnd, ContentEnd -
RestartCount × 4)` is the free-space region used by the in-place
insert / delete splice helpers (`tryAppendCompressed`,
`tryInsertAtCompressed`, `tryDeleteAtCompressed`). The restart
table is located at `ContentEnd - RestartCount × 4`.

**Restart table entry** (4 bytes per group):

```
+----------+--------+-----------+
| Offset   | Count  | Reserved  |
| uint16   | uint8  | uint8     |
+----------+--------+-----------+
```

- `Offset`: byte offset within the page of the group's first
  (restart) entry. Little-endian.
- `Count`: number of entries in this group, in `[1, 255]`. The
  `uint8` width is the hard physical cap; `RestartGroupTarget` is
  bounded to `[1, 255]` correspondingly (see `keyspaces.md
  §Keyspace Descriptor` and `api-surface.md` Options). Groups end
  either at `RestartGroupTarget` entries (the cap), or earlier when
  the builder's split heuristic chooses a natural break — e.g.,
  a key that shares no prefix with its predecessor is a poor
  candidate for delta encoding, so the builder may start a fresh
  group at it rather than spend the 2-byte delta-header overhead
  on negative savings. There is no minimum group size other than 1
  (a single-entry group is the degenerate-but-valid case for keys
  with no prefix sharing); for workloads with systematically zero
  prefix sharing the keyspace should select `RestartGroupTarget =
  1` for the uncompressed variant rather than relying on the
  builder to detect the pattern.
- `Reserved`: zero on write; reserved-byte read policy is the
  project-wide rule in `file-layout.md §Reserved-byte policy
  (project-wide)` — per-page padding bytes are ignored on read
  and kept available for future format extensions.

The variable-group design replaces the prior uniform-K mapping
(`entryIndex / K` derived the group) with explicit per-group counts
read directly from the table. This decouples per-page group structure
from the keyspace's `RestartGroupTarget`: the target is a *builder
hint*, not a per-page invariant — old pages keep their group
structure across `RestartGroupTarget` config changes; new pages use
the new target.

#### Restart entry (full key, first entry of each group)

```
Restart Entry (inline)
+-----------+----------+----------+-----------+-----------+
| CellFlags | KeyLen   | ValueLen | Key bytes | Val bytes |
| uint8     | uint16   | uint32   |           |           |
+-----------+----------+----------+-----------+-----------+
```

#### Delta entry (subsequent entries within a group)

```
Delta Entry (inline)
+-----------+-----------+-------------+----------+---------------+-----------+
| CellFlags | SharedLen | UnsharedLen | ValueLen | UnsharedKey   | Val bytes |
| uint8     | uint16    | uint16      | uint32   |               |           |
+-----------+-----------+-------------+----------+---------------+-----------+
```

`SharedLen` = leading bytes shared with the previous entry in the
same restart group. `UnsharedKey` contains only the bytes after the
shared prefix. Full key = `prevEntry.Key[:SharedLen] || UnsharedKey`.

Per-entry overhead comparison: the restart-entry header is `1 + 2 +
4 = 7` bytes (`CellFlags + KeyLen + ValueLen`); the delta-entry
header is `1 + 2 + 2 + 4 = 9` bytes (`CellFlags + SharedLen +
UnsharedLen + ValueLen`). The delta header costs 2 extra bytes
beyond the restart header, and the delta avoids `SharedLen` bytes
of key data. Net saving per delta entry is `SharedLen - 2` bytes —
positive whenever keys share more than a 2-byte prefix.

**Field ordering — `ValueLen` precedes the key (decode-speed
rationale).** Every fixed-length length field (`KeyLen`, `ValueLen`,
and a delta's `SharedLen` / `UnsharedLen`) sits in the entry header
*before* the variable-length key and value, so the decoder computes
the next entry's start offset from the header alone (`KeyLen +
ValueLen`) without waiting on the key copy — the offset arithmetic
overlaps the copy (instruction-level parallelism). A microbenchmark
isolating this single variable (`internal/page/layout_bench_test.go`)
measures ~24% faster full-leaf decode versus placing `ValueLen`
after the key, for a wash on search; that decode cost is paid by
every non-spliced read and the splice fallback, so the order is
load-bearing, not incidental. (Overflow / nested-tree forms instead
carry a fixed 16-byte trailer after the key — `KeyLen` in the header
still makes the next offset header-computable.)

#### Overflow reference at a restart point (CellFlags bit 0 set)

```
Restart Overflow Reference
+-----------+----------+-----------+----------+----------+
| CellFlags | KeyLen   | Key bytes | OvflPage | TotalLen |
| uint8     | uint16   |           | uint64   | uint64   |
+-----------+----------+-----------+----------+----------+
```

#### Empty-value cell (restart / uncompressed and delta forms)

```
Empty-value entry (restart / uncompressed)
+-----------+----------+-----------+
| CellFlags | KeyLen   | Key bytes |
| uint8     | uint16   |           |
+-----------+----------+-----------+

Empty-value entry (delta)
+-----------+-----------+-------------+---------------+
| CellFlags | SharedLen | UnsharedLen | UnsharedKey   |
+-----------+-----------+-------------+---------------+
```

The value half is absent; the decoded value is empty. This is the
storage form for nested B+tree members (whose values are always
empty) and any plain put of an empty value.

#### Overflow reference at a delta position

```
Delta Overflow Reference
+-----------+-----------+-------------+---------------+----------+----------+
| CellFlags | SharedLen | UnsharedLen | UnsharedKey   | OvflPage | TotalLen |
| uint8     | uint16    | uint16      |               | uint64   | uint64   |
+-----------+-----------+-------------+---------------+----------+----------+
```

### Segregated Leaf (`TypeLeafSegregated`)

The segregated leaf keeps the compressed variant's restart-group
key compression but splits each entry in two: the **entry stream**
(headers + key bytes, packed contiguously from the page front —
the region every search and iteration step touches) and the
**value region** (value bytes, packed contiguously in entry
order, ending at `ValueEnd`). Point lookups and key-only scans
never stride over value bytes; a full-page decode reads two dense
regions instead of one interleaved stream.

```
Segregated Leaf Page
+-----------------------+ offset 0
| Page Header (8 bytes) | Type=TypeLeafSegregated, Count=N
+-----------------------+ offset 8
| RestartCount uint16   |  number of restart groups
| DataEnd      uint16   |  byte offset after the last entry's stream bytes
| ValueEnd     uint16   |  byte offset after the last entry's value bytes
+-----------------------+ offset 14
| Entry 0 (restart)     |  entry stream: headers + key bytes,
| Entry 1 (delta)       |  forward order starting at offset 14
| ...                   |
+-----------------------+ DataEnd
|       free space      |
+-----------------------+ VOff of entry 0
| Value Region          |  value content, packed in entry order,
|                       |  ending exactly at ValueEnd
+-----------------------+ ValueEnd (<= restart table base)
| Restart Table         |  RestartCount × 4 bytes, packed at content end
+-----------------------+ ContentEnd (PageSize - optional 8-byte footer)
```

Entry-stream forms (the restart table is the §Compressed Leaf
4-byte form unchanged):

```
Restart entry
+-----------+--------+--------+-----------+
| CellFlags | KeyLen | VOff   | Key bytes |
| uint8     | uint16 | uint16 |           |
+-----------+--------+--------+-----------+

Delta entry
+-----------+-----------+-------------+--------+--------------+
| CellFlags | SharedLen | UnsharedLen | VOff   | UnsharedKey  |
| uint8     | uint16    | uint16      | uint16 |              |
+-----------+-----------+-------------+--------+--------------+
```

`VOff` is the absolute page offset of the entry's value-region
content. Value LENGTHS are stored nowhere: entry `i`'s content is
`[VOff[i], VOff[i+1])` in ENTRY order, and `[VOff[N-1], ValueEnd)`
for the last entry — so `VOff` is monotone non-decreasing in entry
order with no gaps inside the region. The region is
**END-ANCHORED**: `ValueEnd` is fixed across value splices, and
growth claims free space at the region's LOW end (`VOff[0]` moves
down). A splice that inserts, deletes, or resizes entry `i`'s
value therefore shifts the value bytes and `VOff` fields of
entries `0 .. i-1` (the entries BEFORE it in entry order, which
occupy the low end) and leaves entries `i+1 ..` untouched — or
declines into the rebuild path. The derivation is BY ENTRY ORDER,
never by value address (§Invariants — zero-length values make
adjacent entries share a `VOff`).

Value-region content by `CellFlags`: inline value → the raw value
bytes (length derived, so the interleaved `ValueLen` field does
not exist); overflow value → `OvflPage(uint64) ||
TotalLen(uint64)`; subpage / nested-tree → the `set-keyspace.md`
value-half bytes unchanged; overflow key → the 12-byte key-extent
reference prepended to the above (§Overflow-Key Cells). An empty
value is a zero-length span — `CellFlags` bit 3 (`EmptyValue`) is
never set by the segregated encoder (emptiness is derived, the
flag would be redundant state) and is rejected by the segregated
reader's trust boundary.

`ValueEnd <= restart table base` is the validity rule; the
canonical builder packs the value region flush against the restart
table (`ValueEnd == ContentEnd - RestartCount × 4`, except the
`Count == 0` form below, whose `ValueEnd` is the entry-data
start). A splice that
GROWS the restart table (a new group) moves the whole value region
down by 4 bytes (every `VOff` and `ValueEnd` decrease by 4) or
declines; one that shrinks it leaves a gap above `ValueEnd`, which
persists until the page is next rebuilt — `ValueEnd` is the
explicit source of truth for the last entry's length either way.
The canonical empty page (`Count == 0`, never persisted by the
delete path but valid transiently) has `RestartCount == 0` and
`DataEnd == ValueEnd == 14`. Lookup, iteration, group-size
semantics, the singleton overflow-key group rule, and the
deterministic-encoding invariant are the §Compressed Leaf
contracts unchanged; only the per-entry byte placement differs.

### Uncompressed Leaf (`TypeLeafUncompressed`)

```
Uncompressed Leaf Page
+-----------------------+ offset 0
| Page Header (8 bytes) | Type=TypeLeafUncompressed, Count=N
+-----------------------+ offset 8
| DataEnd      uint16   |  byte offset after the last entry's bytes
| Reserved     uint16   |  zero on write
+-----------------------+ offset 12
| Entry 0               |  entries in forward order starting at offset 12
| Entry 1               |
| ...                   |
+-----------------------+ DataEnd
|       free space      |
+-----------------------+
| Offset Table          |  N × 2 bytes, packed at content end
+-----------------------+ ContentEnd
```

The 2-byte `Reserved` at offset 10 keeps `Entry 0`'s offset at 12,
identical to the compressed variant. Readers and the `Check()` walk
don't need a per-variant entry-start offset. Reserved-byte read
policy is the project-wide rule in `file-layout.md §Reserved-byte
policy (project-wide)`: per-page padding bytes are zero on write
and ignored on read.

Every entry is a full key with no shared / unshared distinction:

```
Entry (inline)
+-----------+----------+----------+-----------+-----------+
| CellFlags | KeyLen   | ValueLen | Key bytes | Val bytes |
| uint8     | uint16   | uint32   |           |           |
+-----------+----------+----------+-----------+-----------+
```

(Overflow form: `ValueLen` replaced by `OvflPage uint64 + TotalLen
uint64`, same as the compressed restart-entry overflow form.
Empty-value form: `ValueLen` and value bytes absent entirely —
`[CellFlags][KeyLen][Key]`, CellFlags bit 3; see the empty-value
cell section above. The 9-byte per-entry overhead figure below is
for non-empty inline values; empty-value cells cost 5 bytes
including the offset-table slot.)

Lookup is a single O(log N) binary search via the offset table. The
uncompressed variant motivation is **operational simplicity**, not
density: per-entry overhead in the uncompressed variant
(`CellFlags + KeyLen + ValueLen + 2-byte offset-table slot = 9`
bytes) equals the compressed delta-entry overhead at zero shared
prefix (`CellFlags + SharedLen + UnsharedLen + ValueLen = 9` bytes,
no per-entry offset). At `SharedLen == 0` the two formats consume
identical per-entry bytes; the only delta is the compressed
variant's 4-byte-per-group restart-table cost (`4 /
RestartGroupTarget` bytes/entry — ~0.67 at the default target 6),
which is negligible.

The uncompressed variant is the right choice when:

- Lookup determinism matters: single O(log N) binary search vs the
  compressed `O(log G + K)` two-phase walk.
- `Prev` cost matters: O(1) via offset table vs the compressed
  `LeafIter` buffered-mode group reload.
- Decode simplicity matters: no `LeafIter` machinery; `Check()` /
  recovery walks per-entry without delta-decode bookkeeping.

For keys with systematic prefix sharing (file paths, composite
indexes, ordered IDs), the compressed variant wins on density — see
§Leaf Density below. For keys that don't share prefixes, the two
variants are density-equivalent and the uncompressed variant is
operationally simpler.

### Leaf Lookup

Both variants expose the same `SearchLeaf(target) → (index, entry,
found)` and `SearchLeafIter(target, …) → (index, entry, found, iter)`
contracts; the variant-specific machinery is encapsulated.

- **Compressed**: phase 1 binary search over restart-table offsets,
  `O(log G)` where G = `RestartCount`. Each probe decodes one
  restart entry's full key (no delta state needed). Phase 2 linear
  scan within the matched group, `O(K)` where K is the matched
  group's `Count` (≤ `RestartGroupTarget`). Total `O(log(N/K) + K)`.
- **Uncompressed**: single O(log N) binary search over the offset
  table.

`SearchLeafIter` is the cursor-friendly form: returns the lookup
result plus a `LeafIter` whose **next** `Next()` call returns the
entry immediately *after* the returned (found-or-successor) entry —
i.e. the iterator is positioned past the result, ready to stream
forward without re-emitting the entry the caller just received. It
carries the delta-decode state accumulated during the scan, so
Cursor `Seek` / `SeekGE` avoid a second group walk.

### Cursor Iteration

The cursor delegates leaf traversal to a `LeafIter` exposed by the
page package. `LeafIter` is a bidirectional iterator that owns the
decode state for the current leaf; the cursor stack stays slim, and
the per-leaf-format machinery (compressed vs uncompressed) stays
encapsulated. `LeafIter` operates in two modes:

- **Forward-streaming mode** (initial state for compressed leaves).
  Maintains a `keyBuf []byte` carrying the current full key. `Next()`
  reads the next delta entry's `SharedLen` and `UnsharedKey` directly
  from the page, truncates `keyBuf` to `SharedLen`, and appends
  `UnsharedKey` in place — amortized O(1) without per-step
  allocation. Crossing into a new restart group reads the group's
  first (full) restart key directly from the page.
- **Buffered mode** (entered on the first `Prev()` or `At()` call
  on a compressed leaf). Decodes the current restart group into
  `bufEnts []LeafEntry + bufKeys []byte`. All subsequent
  `Next`/`Prev`/`At` calls serve from the buffer; group-boundary
  crossings reload the adjacent group. The buffer storage
  (`bufEnts`, `bufKeys`, and the `keyBuf`) is passed in to
  `IterAtForReuse` / `IterForReuse` and returned via `KeyBuf` /
  `BufKeys` / `BufEnts` so the cursor reclaims it across leaf
  transitions — per-cursor allocation amortizes to zero in the
  steady-state cursor loop.

Uncompressed leaves don't need the streaming/buffered distinction:
`Next`/`Prev`/`At` are all O(1) via the offset table.

This unifies what the original spec called the "key reconstruction
buffer" and the "group cache" behind a single iterator interface,
so all leaf-walking callers — `btree.Cursor`, `btree.Get` (via
`SearchLeaf`), and the range-delete scanner — share decode
infrastructure.

### Leaf Density

Depends on the ratio of shared-prefix length to total key length.
For 200-byte keys sharing a 150-byte common prefix + 50-byte values
at 4 KB (`ContentEnd = 4088` with checksums):

| Format | Bytes/entry (avg) | Entries/page | Improvement |
|--------|-------------------|-------------|-------------|
| Uncompressed (`TypeLeafUncompressed`) | ~259 | ~15 | baseline |
| Compressed (`TypeLeaf`, default target K=6) | ~134 | ~30 | 1.9× |
| Compressed (`TypeLeaf`, target K=16) | ~118 | ~34 | 2.2× |

For keys with **no** shared prefix (50-byte random keys + 50-byte
values), the variants are essentially identical: compressed
~109.7 bytes/entry (≈37 entries/page) at the default target vs
uncompressed ~109 bytes/entry (≈37 entries/page). The sub-byte
delta per entry is the compressed variant's per-group
restart-table overhead (`4 / RestartGroupTarget` bytes amortized:
~0.67 at target 6, ~0.25 at 16) — not material.

So `RestartGroupTarget = 1` (uncompressed) is **not** chosen for
density on random-key workloads — both formats are
density-equivalent there — but for the operational properties listed
in §Uncompressed Leaf above (single-phase O(log N) lookup, trivial
`Prev`, simpler `Check()` walk).

### Insert and Delete

Insert and delete within a leaf splice the page in place when the
resulting layout fits — `tryAppendCompressed`,
`tryInsertAtCompressed`, `tryDeleteAtCompressed` for the compressed
variant; `ucTryAppend`, `ucTryInsertAt`, `ucTryDeleteAt` for the
uncompressed. Inserting between two compressed delta entries
re-encodes the successor entry's delta against the new predecessor
and may shift the containing group's boundaries; the splice helpers
return false when the layout impact crosses a boundary in a way the
local rewrite can't predict, at which point the caller falls back to
the full decode-and-rebuild path (the `TryInsertAt` / `TryDeleteAt`
dispatchers' callers in `internal/btree`).

The restart table is rebuilt only when group composition changes —
not on every insert / delete. Uncompressed leaves rebuild only the
offset table; the per-entry data shifts but no key re-encoding
occurs.

**Delete-side rebuild fallback.** A delete's keep-set is NOT
removal-monotone under the canonical builder: re-packing the
survivors re-aligns restart-group boundaries (each shifted boundary
stores a full key where a delta sufficed), and the rebuild's variant
migration (a page whose on-disk variant differs from the configured
`RestartGroupTarget`) can inflate a delta-heavy page by far more
than one page. When the canonical decode-and-rebuild of a delete's
keep-set does not fit one page, the delete falls back to
native-variant splices of the original page bytes
(`TryDeleteAtNative`): a splice delete always shrinks (the
compressed splice's shared-prefix triangle-inequality bound; the
uncompressed sorted-array delete), so removing entries from a
fitting page in its own variant always fits. The page keeps its
on-disk variant and group structure — variant migration on the
delete path is opportunistic, never load-bearing. Consequence, and
the invariant the fallback restores: **the leaf keep-set rebuild
never fails for encoding reasons and never splits the leaf**. The
claim is deliberately scoped to the rebuild step: a delete may
still grow a page when a fits-but-larger variant migration is
taken, and a delete's merge/redistribute/root-collapse machinery
changes tree shape by design (its own encoding-infeasibility
handling is governed by `range-delete.md` §Invariants, not this
clause).

### Leaf Split

On overflow, the leaf is split into two halves at a *group boundary*
(compressed) or *entry boundary* (uncompressed) chosen by split
bias — typically 50% of data bytes. The `FindSplitGroup` /
`FindSplitIndex` helpers walk the restart / offset table for the
boundary closest to the bias target. **Tiebreak**: when two adjacent
boundaries are equidistant from the bias target, the lower-index
boundary wins. Each half is then encoded independently with fresh
group structure (compressed) or fresh offset table (uncompressed).

Boundary keys (last key of left leaf, first key of right leaf) are
reconstructed from the source page (full decode for the boundary
positions only — not the whole leaf; a boundary entry that is an
overflow-key cell materializes inline + extent bytes). Separator
computation for the parent branch uses these full keys (see
§Separator Computation (Cross-Level Truncation)).

**Deterministic encoding invariant** (consequence of the tiebreak +
the per-page group-target policy): the **same encoder version**
given the same input sequence, `RestartGroupTarget`, `PageSize`,
and `PageChecksum` configuration must produce byte-identical pages.
This is the property `Check()` repair, recovery testing, and any
future cross-process determinism tooling rely on; any encoder
change that breaks within-version determinism is a format break in
the same sense as a layout change. The spec deliberately does *not*
mandate byte-identical output across encoder versions: the §Compressed
Leaf "natural break" heuristic and the "typically 50%" split bias
are policy knobs the encoder may tune over time; what's pinned is
that any single deployed encoder produces the same bytes for the
same input, so a `Check()` repair re-encoding a page yields the
same bytes the original writer would have written.

## Overflow-Key Cells

Keys are not bounded by the page size. A key whose FULL length
exceeds the **inline threshold `T`** is stored as an overflow-key
cell with a fixed cut at byte `T`: the extent holds `key[T:]`, the
resident inline bytes are `key[0:T]` in a leaf and in a plain
branch, `key[PrefixLen:T]` in a segregated branch (§Branch form),
and a 12-byte **key-extent reference** — `KeyExtPage (uint64)`,
the first page of an §Overflow Page run holding `key[T:]`, and
`KeyTotalLen (uint32)`, the FULL key length — is carried per the
holding layout's form (after the inline bytes in the interleaved
leaf and both branch layouts; at the front of the entry's
value-region content in the segregated leaf). The trigger is the
full key length, never a page-relative portion — a suffix-relative
trigger would flip a separator between inline and overflow form as
the page prefix changes, re-creating the extent-rewrite-on-move
problem the fixed cut removes. Keys `<= T` use the layout's inline
encodings byte-for-byte.

### The inline threshold `T`

`T` is a pure function of `(PageSize, PageChecksum)` — one shared
constant across every layout variant, never per-layout (a
per-layout `T` would move the extent cut when a key crosses layout
boundaries, re-creating the extent-rewrite-on-move problem the
fixed cut removes):

```
T = (PageSize - 76) / 2        with PageChecksum
T = (PageSize - 68) / 2        without
```

| Page Size | `T` (with checksum) | `T` (without) |
|-----------|--------------------:|--------------:|
| 4 KB | 2010 | 2014 |
| 8 KB | 4058 | 4062 |
| 16 KB | 8154 | 8158 |
| 64 KB | 32730 | 32734 |

The governing constraint is the **per-layout split-feasibility
floor** (§Invariants): every supported BRANCH layout holds TWO
worst-case overflow-key cells (zero shared prefix where the layout
has one) within one page, and every supported LEAF layout holds
ONE maximal-form entry (overflow key + overflow value), at every
page size. The constants above satisfy both floors with slack —
the worst branch budget is the segregated branch at
`2T + 74 <= PageSize` with `PageChecksum` (header 8 + leftmost 8 +
PrefixLen/Reserved 4 + directory 3×2 + 2 × (T inline + 12 extent
ref) + child array 16 + footer 8); the plain branch needs
`2T + 72`; the leaf floors need only `T + ~60`. (TWO maximal leaf
entries per page do NOT fit — `2T + 86..96 > PageSize` — and are
not required: leaf splits may produce single-entry leaves.) The
exact constants and each layout's floor feasibility are pinned by
test; the floors are the contract, the constants are the chosen
fixed point. `T` fits 15
bits at every page size (the plain-branch directory's overflow
marker occupies `KeyLen` bit 15). The extent cut sits EXACTLY at
byte `T` of the full key — the resident inline bytes are
`key[0:T]` (leaf, plain branch) or `key[PrefixLen:T]` (segregated
branch), never a different cut — so encoding stays a pure function
of the input and the page's `PrefixLen` (the §Leaf Split
deterministic-encoding invariant).

### Leaf forms

`CellFlags` bit 4 (`OverflowKey`) modifies only the key half of an
entry: `KeyLen` is repurposed as the inline length (always `T`).
In the **interleaved** leaf (and the uncompressed variant) the
inline key bytes are followed by the 12-byte key-extent reference,
and the value half — inline, overflow, empty, subpage, or
nested-tree per bits 0–3 — is unchanged in FORM, positioned after
the key-extent reference (where the value half of an inline cell
sits after the key bytes). The subpage and nested-tree-reference
value halves (`set-keyspace.md §Subpage Format` / §Nested B+tree
Reference Cell) compose the same way: their bytes are
byte-identical to the inline-key case, shifted by the 12-byte
reference. In the **segregated** leaf the key stream stays pure
key bytes — the 12-byte key-extent reference is instead PREPENDED
to the entry's value-region content (read only when a comparison
ties through all resident bytes), and the value half's bytes
follow it in the value region unchanged in form. Diagrammed
interleaved forms:

```
Overflow-Key Restart Entry (inline value)
+-----------+--------+----------+----------------+------------+-------------+-----------+
| CellFlags | KeyLen | ValueLen | Key bytes (=T) | KeyExtPage | KeyTotalLen | Val bytes |
| uint8     | uint16 | uint32   |                | uint64     | uint32      |           |
+-----------+--------+----------+----------------+------------+-------------+-----------+

Overflow-Key Restart Entry (overflow value)
+-----------+--------+----------------+------------+-------------+----------+----------+
| CellFlags | KeyLen | Key bytes (=T) | KeyExtPage | KeyTotalLen | OvflPage | TotalLen |
| uint8     | uint16 |                | uint64     | uint32      | uint64   | uint64   |
+-----------+--------+----------------+------------+-------------+----------+----------+

Overflow-Key Restart Entry (empty value)
+-----------+--------+----------------+------------+-------------+
| CellFlags | KeyLen | Key bytes (=T) | KeyExtPage | KeyTotalLen |
| uint8     | uint16 |                | uint64     | uint32      |
+-----------+--------+----------------+------------+-------------+
```

Every fixed-length field stays in the header or at a
header-computable offset, so the next-entry offset is computable
without touching the variable bytes — the §Compressed Leaf
field-ordering (decode-speed) property is preserved: the
interleaved key half contributes a fixed `KeyLen + 12`.

Segregated overflow-key restart entry — the entry-stream half is
the ordinary segregated restart form with `KeyLen == T`; the
value region carries the reference plus the value content:

```
Segregated Overflow-Key Restart Entry
entry stream:  | CellFlags | KeyLen | VOff   | Key bytes (=T) |
               | uint8     | uint16 | uint16 |                |
value region:  | KeyExtPage | KeyTotalLen | value content per bits 0-3 |
               | uint64     | uint32      | (inline bytes / OvflPage+
               |            |             |  TotalLen / subpage / ...) |
```

**Restart-group rule.** An overflow-key entry is always a restart
entry and always a SINGLETON group (`Count == 1` in its
restart-table entry); the following entry, if any, starts a fresh
group with a full key. Delta entries never carry an overflow key
and never share against one — in both compressed leaf layouts. In
the uncompressed variant (`TypeLeafUncompressed`) the overflow-key
entry form is the interleaved one minus group bookkeeping.

**Derivable-length read policy.** The inline length is a constant
of the page configuration — leaf `KeyLen == T`; plain-branch
low-15 `KeyLen == T`; segregated-branch marked heap span
`== (T - PrefixLen) + 12` — stored explicitly for decode
uniformity. The read trust boundary verifies the equality
(`LeafReader.Validate` and the branch decoders): a divergent value
is structural corruption, treated exactly like a restart-table
`Count == 0` — never "trusted field", never silently recomputed.

### Branch form

Defined per variant in §Plain Branch (directory `KeyLen` bit 15)
and §Segregated Branch (child-pointer bit 63). The extent cut is
the SAME fixed cut as the leaf form — `separator[T:]` —
independent of any page's prefix `P`; only the segregated inline
slice is page-relative (`separator[PrefixLen : T]`,
reconstructible as `P || Inline`; the plain branch stores
`separator[0:T]` whole). Separator computation at split
(§Separator Computation) is unchanged — it operates on full keys,
materializing boundary keys from their extents when the boundary
entries are overflow-key cells. A separator move
(split/merge/redistribute promoting or demoting a separator
between pages, any re-encode under a different `P`, and any move
between layout variants) re-slices the inline bytes from the
reconstructed first-`T` prefix and carries the extent BY
REFERENCE; the extent is written once when the over-`T` separator
is first created and retired through the RPL when the separator
is removed or replaced.

### Comparison

Stated over FULL keys. The stored key's first `T` bytes are always
resident: `key[0:T]` directly in a leaf or plain branch;
`P || Inline` in a segregated branch (no extent read to
reconstruct). If the compared full key diverges
from `stored[0:T]` within its first `T` bytes, or has length
`<= T` (a full match then makes it a strict prefix of the stored
key — strictly less), the order is decided from resident bytes.
The extent is read exactly when the compared key's length exceeds
`T` AND `compared[0:T] == stored[0:T]`. Long-key workloads with
distinct prefixes therefore never pay an extent read on lookup;
the read is proportional to actual shared-prefix depth.

### Lifecycle, iteration, integrity

Key extents are §Overflow Page runs and follow the value-overflow
lifecycle rules everywhere: CoW page rewrites carry the reference
without copying the run; delete, keep-set rebuild, merge, and
range-delete retire the run through the RPL exactly as they retire
value runs; `Check()` walks key extents with the same
run-attribution rules as value runs; `Compact`/`CopyTo` relocate
them like value runs. Cursor key materialization copies
inline + extent bytes into the iterator's `keyBuf`; the borrowed
byte-slice contract (`overview.md` — keys valid until the next
cursor op) is unchanged.

## Overflow Page

Overflow pages are physically contiguous runs that store large
values and the extent portion of overflow keys (§Overflow-Key
Cells). The first (head) page carries the standard 8-byte page
header with `AdditionalPages` set to the number of follower pages,
followed — when `PageChecksum` is enabled — by an 8-byte XXH3-64
**whole-run digest** (`checksums.md §Overflow-Run Digest`); the
extent bytes start immediately after (head offset 16 with
`PageChecksum`, 8 without) and continue through the follower
pages, which carry no header, no footer, and no digest. Nothing
interrupts the extent: the stored bytes are ONE contiguous byte
range, so a reader returns them as a single borrowed slice from
the mmap (`api-surface.md §Byte Slice Ownership`).

Total extent capacity for a run of `1 + N` pages:
`(PageSize - 16) + N × PageSize` bytes with `PageChecksum`,
`(PageSize - 8) + N × PageSize` without. The extent LENGTH is
supplied by the referencing cell — `TotalLen` for a value run,
`KeyTotalLen - T` for a key-extent run — never stored in the run
itself. Run pages carry NO per-page checksum footers — the
whole-run digest is the run's entire integrity cover. It is
computed over the run's FULL content range (head content start
through the last follower's end — i.e. the capacity above), not
over the extent length alone: that makes the run self-verifiable
from its head plus `AdditionalPages`, with no dependence on the
referencing cell (the proactive scrub in
`background-maintenance.md` verifies runs standalone). Slack
bytes past the extent length in the last page are ZERO ON WRITE —
unconditionally, checksums on or off, so a run image is a pure
function of its extent bytes; with checksums on, the whole-range
digest is what enforces it.

## NUL-escape encoding (composite keys)

This encoding is used wherever multiple lex-ordered columns are
concatenated into a single byte key — currently secondary indexes
(see `indexing.md`).

- Within each column's bytes, every `0x00` is escaped to `0x00 0xFF`.
- After each column's escaped bytes, append a `0x00 0x00`
  terminator.
- The full key shape is statically determined by `IndexDecl.Unique`
  (per `indexing.md §Storage Layout`):
  - **Unique indexes:** the key is `(escapedCol 0x00 0x00)+` — just
    the concatenated escaped column tuple with terminators.
  - **Non-unique indexes:** the key is extended with `escapedPK
    0x00 0x00` — the escaped primary key plus a final terminator.
    For SetKeyspace indexes the PK is the compound `escape(setKey)
    || 0x00 0x01 || escape(setValue)` per `set-keyspace.md §Indexes
    on SetKeyspaces`.

The encoding is **prefix-free**: no escaped column is a prefix of
another, and the column terminator `0x00 0x00` never appears inside
an escaped column (every internal `0x00` is followed by `0xFF`).
Concatenated columns sort lex-correctly regardless of contents,
including columns with embedded NULs.

**Worked example.** Two tuples to encode:

| Tuple | Col A | Col B | Encoded bytes |
|-------|-------|-------|---------------|
| T1 | `[]` (empty) | `[0x00]` | `00 00`  `00 FF 00 00` |
| T2 | `[0x00]` | `[]` (empty) | `00 FF 00 00`  `00 00` |
| T3 | `[0x00, 0xFF]` | `[0x00]` | `00 FF FF 00 00`  `00 FF 00 00` |

Byte-wise comparison yields `T1 < T2 < T3`, matching the lex order
of the original tuples. A decoder finds column boundaries
unambiguously by scanning for the `00 00` terminator.

A distinct separator `0x00 0x01` is used inside SetKeyspace
compound-PK encodings to separate the set key from the set value —
see `set-keyspace.md §Indexes on SetKeyspaces`.

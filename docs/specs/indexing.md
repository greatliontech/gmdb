# Indexing

gmdb maintains secondary indexes on keyspaces declaratively. The
caller declares one or more indexes per keyspace at open time,
supplying an extractor function that produces index entries from a
row. The engine applies index changes inside every write
transaction that modifies the keyspace, atomic with the row write.

Scope:
- `IndexDecl`, `IndexColumn`, `IndexCoveringColumn`, `IndexEntry`,
  `IndexExtractor`.
- Drift guard (schema hash + version tag).
- Column encoding (the NUL-escape scheme is defined in
  `page-formats.md §NUL-escape encoding`).
- On-disk storage layout (per-keyspace registry + engine-internal
  index keyspaces).
- Unique, covering, and partial-index semantics.
- Lookup API surface (`Lookup`, `LookupKeys`, `Range`, `Prefix`,
  `Get`, `Stats`).
- Write path (Put / Delete / Cursor.Delete with index
  maintenance).
- `RebuildIndex` recovery loop.
- Bulk operations interaction (DeleteRange fallback).
- Open semantics — `OpenKeyspace` vs `OpenKeyspaceReadOnly` and
  same-tx re-open rules.
- Indexes on SetKeyspaces.
- `DropIndex` and `Index.Stats`.

Depends on / interacts with:
- `keyspaces.md` for `IndexRegistryRoot` and the `Kind = 2`
  internal-keyspace discriminator.
- `page-formats.md` for the NUL-escape encoding.
- `set-keyspace.md §Indexes on SetKeyspaces` for compound-PK
  encoding.
- `transactions.md` for cursor-stability across CoW + rebalance
  during per-row delete loops.
- `bulkload.md` for sorted-input index population, including the
  unique-violation detection rule.
- `api-surface.md` for Go-level signatures and the
  `IndexFingerprintError` struct.

## Invariants

Invariant: kind=clause-explicit;
  property=On opening a keyspace for write, the supplied
    `IndexDecl` set must match the keyspace's stored registry
    exactly: same set of names, same column tuple per index,
    same `Unique` flag, same `Version`, and the same
    schema-hash. Any mismatch surfaces as
    `ErrIndexFingerprintMismatch` or `ErrIndexUnknown` /
    `ErrIndexExtractorRequired` at open time, before any work;
  from=this spec §Drift Guard + §Open Semantics;
  violation=Opening a drifted keyspace silently runs the
    extractor under one schema while the on-disk entries are
    keyed under another — every Put either inserts garbage
    duplicates or fails non-deterministically; Get returns
    wrong rows.

Invariant: kind=clause-explicit;
  property=Every `Put`, `Delete`, and `Cursor.Delete` on an
    indexed keyspace applies the row write and all index entry
    deltas atomically within the same CoW transaction;
  from=this spec §Write Path: Atomic Index Maintenance;
  violation=Partial index maintenance leaves the index pointing
    at a stale row (false-positive lookup) or omits an entry
    the row should have (false-negative lookup) — silent
    correctness failure that survives until the next
    `Check(CheckIndexes)` pass.

Invariant: kind=clause-explicit;
  property=A `Put` that would introduce a duplicate key in any
    unique index aborts with `ErrIndexUniqueViolation` *before*
    writing the row or any index entries. The check happens
    against the candidate-set so two entries with the same
    key produced by a single extractor invocation are also
    detected;
  from=this spec §Unique Indexes;
  violation=Either accepting the duplicate (corrupts uniqueness
    contract) or writing partial state (row written, index
    write deferred) breaks the "row + index always agree"
    invariant.

Invariant: kind=clause-explicit;
  property=Index entries live in engine-internal keyspaces
    (`Kind = 2`) referenced indirectly through the per-keyspace
    index registry. Internal keyspaces never appear in the user
    keyspace B+tree directly and cannot be opened via the user
    API;
  from=this spec §Storage Layout;
  violation=Exposing internal keyspaces lets user code bypass
    the registry-managed maintenance contract, producing
    index/row drift the engine cannot detect.

Invariant: kind=clause-explicit;
  property=Schema-hash inputs are exclusively byte sequences
    with explicit `uvarint` length prefixes — no `gob`, no
    JSON, no struct layout — so the hash is deterministic
    across Go versions, build flags, and host architectures;
  from=this spec §Drift Guard;
  violation=A non-deterministic schema-hash misclassifies
    fingerprint matches between processes or Go versions,
    silently rejecting legitimately-identical schemas or
    accepting drifted ones.

Invariant: kind=clause-explicit;
  property=Within one transaction, a second `OpenKeyspace` for
    the same name returns the **same** `*Keyspace` handle iff
    every hashable input matches the first call's set (names,
    `Unique` flags, schema hashes, `Version`s, and — for
    typed indexes — encoder IDs). Any mismatch returns
    `ErrKeyspaceAlreadyOpen`. Indexes declared at first open
    are pinned for the rest of the transaction;
  from=this spec §Open Semantics (Re-opening);
  violation=Returning two distinct handles with conflicting
    extractor sets makes index maintenance non-deterministic
    on writes through either handle.

Invariant: kind=entailed;
  property=An extractor that returns an empty (or `nil`) slice
    for a row produces no index entries for that row; old
    index entries (from a previous value of the same row) are
    deleted on update — the entry diff is `old \ new` for
    deletes and `new \ old` for inserts;
  from=entailed: partial-index semantics + atomic maintenance
    (this spec);
  violation=Treating "empty slice" and "nil" differently makes
    the partial-index contract surprising; failing to diff
    leaves stale index entries (false-positive `Lookup`).

Invariant: kind=entailed;
  property=`Lookup` is the canonical query API for matched
    `(pk, value)`; `LookupKeys` is the cost-sensitive escape
    hatch that returns raw PKs without back-lookup or
    covering decode. `Lookup` silently skips index entries
    whose back-lookup fails to find the PK (a corruption
    signal reported via `Check()`); `LookupKeys` does not
    probe and yields every entry's PK unconditionally;
  from=this spec §Lookup API godoc;
  violation=Confusing the two would mis-implement either: a
    `Lookup` that surfaces broken entries panics user code on
    every corruption signal; a `LookupKeys` that probes the
    row keyspace defeats its cost-sensitive contract.

Invariant: kind=entailed;
  property=Every engine-internal `Kind = 2` keyspace descriptor
    is reachable via **exactly one** user-keyspace's index
    registry sub-tree, never via the top-level keyspace B+tree
    path used by `ListKeyspaces`, and never via two distinct
    parent keyspaces. The Kind=2 descriptor's lifetime is
    bounded by its parent keyspace's lifetime: parent
    `DeleteKeyspace` retires the Kind=2 descriptor (per
    `api-surface.md §Keyspace API DeleteKeyspace` three-subtree
    retirement); `DropIndex` retires it standalone;
  from=entailed: this spec §Storage Layout (registry-only
    reachability) + `keyspaces.md` invariant #4
    (`ErrKeyspaceReserved` + `ListKeyspaces` filter) — neither
    states the one-parent uniqueness, but two-parent
    reachability would make `DeleteKeyspace` of parent₁ retire
    pages parent₂'s registry still references;
  violation=A Kind=2 descriptor surfacing in `ListKeyspaces`,
    addressable via the user `OpenKeyspace` surface, or
    referenced by two parent keyspaces' registries lets
    `DeleteKeyspace(parent₁)` retire pages a Kind=2 descriptor
    still references from parent₂ — every subsequent `Lookup`
    on parent₂'s indexes reads freed-and-reallocated pages,
    yielding wrong rows or page-checksum failures.

Invariant: kind=entailed;
  property=`DropIndex` removing the last declared index on a
    keyspace resets `desc.IndexRegistryRoot` to `0` and retires
    the (now-empty) registry sub-tree pages in the same write
    transaction. The registry sub-tree's leaf-entry count
    equals the number of declared indexes — never a non-zero
    root pointing at an empty leaf;
  from=entailed: `keyspaces.md` invariant #7's biconditional
    (`IndexRegistryRoot == 0` iff no indexes) already entails
    the canonical-representation direction; what no clause
    states is the **operational rule** that `DropIndex` of the
    last index must both reset the root field AND retire the
    sub-tree pages — a half-step (reset root, leak pages, or
    retire pages, leave root non-zero) leaves #7's iff
    momentarily false mid-transaction;
  violation=`DropIndex` that resets the root without retiring
    the sub-tree pages leaks every registry leaf page; the
    reverse (retire pages, leave the root non-zero) leaves
    `desc.IndexRegistryRoot` pointing at freed-and-
    reallocatable pages — a subsequent `CreateKeyspace`-with-
    indexes path checking `IndexRegistryRoot == 0` to decide
    whether to allocate would write into a leaf the descriptor
    still references, but later `OpenKeyspace` registry walks
    see a non-zero root pointing at a now-stale page, yielding
    wrong index lookups.

## Overview

```go
// IndexDecl describes one secondary index on a byte-oriented keyspace.
type IndexDecl struct {
    Name     string             // unique within the keyspace
    Columns  []IndexColumn      // ordered; concatenated lex-safely
    Covering []IndexCoveringColumn   // optional; stored in the index value
    Unique   bool               // engine rejects extractor-produced duplicates
    Version  string             // user-supplied; bump after extractor-logic changes
    Extract  IndexExtractor
}

type IndexColumn struct {
    // Name is a semantic anchor for the column: it identifies the
    // logical role of this position in the column tuple and contributes
    // to the index's schema-hash fingerprint. The Name is never
    // interpreted by the engine at read time — column storage is
    // purely positional. Renaming a column changes the schema hash and
    // forces RebuildIndex. Reusing a name for a column whose semantic
    // content has changed is the user's responsibility — bump Version
    // in that case (see Drift Guard).
    Name string
}

type IndexCoveringColumn struct {
    Name string // same semantics as IndexColumn.Name
}

type IndexEntry struct {
    Cols  [][]byte // one byte slice per IndexColumn; lex-safe encoded by caller
    Cover [][]byte // one per IndexCoveringColumn (omit when Covering is nil)
}

// IndexExtractor produces zero or more IndexEntry values for a row.
// Returning a nil slice or a zero-length slice both signal "do not
// index this row" (partial-index semantics) and are equivalent.
type IndexExtractor func(key, value []byte) []IndexEntry
```

For typed callers, `TypedIndex[K, V, IK]` wraps `IndexDecl` and
generates column bytes automatically from a typed `Encoder[IK]` —
see `typed-keyspaces.md`.

## Index Declaration

Indexes are declared at the call that opens the keyspace for write
access. Every transaction that opens the same keyspace for write
must supply matching `IndexDecl`s — same name set, same column
specs, same `Unique`, same `Version`. Mismatch surfaces as
`ErrIndexFingerprintMismatch` at open time.

Duplicate `IndexDecl.Name` values in one `OpenKeyspace` call's
variadic slice are rejected with `ErrIndexExists` naming the
offending duplicate. Index names are keys in the schema hash and
in the on-disk registry — duplicates would either collide on the
registry write or render the recovery-loop's linear search
non-deterministic.

## Drift Guard: Schema Hash + Version Tag

For each declared index, the engine computes a deterministic
**schema hash**:

```
xxhash64(
  uvarint(len(index.Name)) || index.Name ||
  uvarint(len(Columns)) || for each col: uvarint(len(Name)) || Name ||
  uvarint(len(Covering)) || for each col: uvarint(len(Name)) || Name ||
  uint8(Unique)
)
```

Every string input — `index.Name`, column names, covering names —
is uvarint-length-prefixed. Without a prefix on `index.Name`, the
two distinct decls

```
A: Name="ab",     Columns=[{Name:""}], Covering=[{Name:""}], Unique=true
B: Name="ab\x01", Columns=[],          Covering=[{Name:""}], Unique=true
```

both encode to the byte sequence `61 62 01 00 01 00 01` (7 bytes,
verifiable by hand), so xxhash64 returns the same value for two
structurally different indexes — a collision. The boundary between
`Name` and `uvarint(len(Columns))` is undetectable when `Name`'s
trailing bytes can mimic a uvarint length. Uniform uvarint-
prefixing is the minimal injective encoding consistent with the
§Drift Guard "exclusively `uvarint` length prefixes" clause-
explicit invariant. (Chunk-7.2 spec amendment; the original
grammar omitted the `Name` prefix.)

The schema hash + the user-supplied `Version` string are stored
on disk in the per-index registry entry. At Open, the engine
compares the **supplied** schema hash and version against the
**stored** ones. Any mismatch returns an
`ErrIndexFingerprintMismatch` value (wrapped in
`*IndexFingerprintError`, see `api-surface.md`) whose error
message names (a) the drifted index, (b) which field differed
(`schema-hash` vs `version`), and (c) the stored and supplied
values:

```
gmdb: index "by_repository" fingerprint mismatch (schema-hash):
  stored=0x3f2a... supplied=0xc104... — caller must RebuildIndex
```

The caller's recovery path is `tx.Indexes().Rebuild` — see Rebuild
below.

The schema hash catches structural drift (column add / remove /
reorder, unique flag flipped, covering changes). The user
`Version` tag catches extractor-logic drift that the engine
cannot inspect (e.g. the extractor now masks a column, returns
entries in a different order, or applies a different
partial-index predicate). Bump `Version` after any extractor
change that produces different output for the same input.

The engine never auto-rebuilds. Auto-rebuild would silently
double the cost of an Open after a deploy and obscure the
schema change in operational logs.

The schema-hash inputs are exclusively byte sequences with
explicit `uvarint` length prefixes — no `gob`, no JSON, no
struct layout — so the hash is deterministic across Go
versions, build flags, and host architectures.

## Column Encoding

`IndexEntry.Cols[i]` is opaque, lex-ordered bytes. The caller
is responsible for producing encodings whose byte order matches
the desired index order (e.g. big-endian for ordered numerics).

The engine concatenates columns into a single index key using
the NUL-escape encoding defined in `page-formats.md §NUL-escape
encoding`:

- Within each column's bytes, every `0x00` is escaped to
  `0x00 0xFF`.
- After each column's escaped bytes, append a `0x00 0x00`
  terminator.
- The full index key is the concatenation of escaped columns +
  their terminators, followed (for non-unique indexes) by the
  escaped row PK + a final `0x00 0x00`.

The typed layer (`TypedIndex[K, V, IK]`) automates lex-safe
encoding via stable `Encoder[T]` implementations — see
`typed-keyspaces.md`.

## Storage Layout

Each keyspace has its own per-keyspace **index registry** — a
B+tree rooted at `IndexRegistryRoot` in the keyspace descriptor
(`keyspaces.md`). Keys are index names; values are the per-index
descriptor:

```
Index Registry Entry (value bytes)
+----------------+----------------------------------+
| SchemaHash     | uint64                           |
| Unique         | uint8                            |
| Padding        | [7]byte                          |
| Root           | uint64    (index B+tree root)    |
| Count          | uint64    (entries in the index) |
| UserVersionLen | uint16                           |
| UserVersion    | bytes                            |
| ColumnCount    | uint16                           |
| For each col:                                     |
|   NameLen      | uint16                           |
|   Name         | bytes                            |
| CoveringCount  | uint16                           |
| For each col:                                     |
|   NameLen      | uint16                           |
|   Name         | bytes                            |
+----------------+----------------------------------+
```

Variable-length. Stored as a single byte-string value in the
index registry tree. Padding after the `Unique` byte aligns the
subsequent `Root` / `Count` uint64s.

Each index's data lives in its own engine-internal keyspace
descriptor (`Kind = 2`) referenced indirectly through `Root` in
the registry entry. Internal keyspaces do not appear in the
user keyspace B+tree directly — their descriptors are reachable
only via the parent keyspace's index registry. This keeps the
user-facing keyspace namespace clean.

Index entries are stored as plain B+tree key-value pairs:

- **Unique index.** key = concatenated lex-safe columns; value
  = `(PK bytes, optional Covering bytes)`.
- **Non-unique index.** key = concatenated lex-safe columns +
  escaped PK; value = optional covering tuple.

The `Count` field on the index descriptor is maintained
incrementally on Put / Delete. `Stats()` returns it in O(1).

## Unique Indexes

When `Unique` is true, the engine rejects extractor output that
would introduce a duplicate index key. `Put` on the indexed
keyspace returns `ErrIndexUniqueViolation` (with the index
name) instead of writing the row.

Implementation: before writing index entries, the engine probes
each new index key. If found, abort with
`ErrIndexUniqueViolation`. The row write does not happen — the
caller's `Put` returns the error and the transaction can
`Rollback()` or continue with other work.

A single extractor invocation may return multiple `IndexEntry`
values. If two of those entries produce the same index key for
a unique index, the `Put` is rejected with
`ErrIndexUniqueViolation` naming the offending key — the row
is not written, no index entries are written. The check happens
against the candidate-set, so the collision is detected even
when the index keyspace is empty.

Unique indexes naturally model partial-unique constraints by
combining with extractor filtering: the extractor returns
entries only for rows matching the condition; uniqueness is
enforced over the filtered set.

## Covering Indexes

When `Covering` is non-empty, the index entry value carries the
covering columns (in declaration order, concatenated with the
same NUL-escape scheme used for keys). `Lookup` returns
covering bytes directly, skipping the back-lookup to the row
keyspace.

A covering column declaration is identified by its `Name`; the
extractor populates `IndexEntry.Cover[i]` with the
corresponding lex-safe bytes. The schema hash includes covering
column names in declaration order, so adding / removing /
reordering covering columns triggers
`ErrIndexFingerprintMismatch`.

**Update rewrites covering.** The covering payload is extracted
from the ROW VALUE, so replacing a row's value can change the
stored covering bytes even when every index key stays the same.
Index maintenance therefore diffs VALUES as well as keys: an
entry present in both the old and new candidate sets whose
encoded value differs is rewritten in place (no delete —
entry count unchanged; the unique probe skips it, since the
on-disk hit at that key is the row's own old entry and the
overwrite is benign).

Invariant: kind=clause-explicit;
  property=After any row mutation commits, every covering index
    entry's stored payload equals the payload extracted from
    the row's CURRENT value;
  from=this spec §Covering Indexes (update rewrites covering);
  violation=Put(k, v1) then Put(k, v2) with an unchanged index
    key: Lookup serves v1's covering bytes forever while the
    row keyspace serves v2 — a wrong-result read the checker
    reports as FingerprintDrift.
    (Enforced by the value-diff in buildReplacePlans; pinned by
    TestCoveringValueRewrittenOnUpdate /
    TestByteCoveringRewrittenOnUpdate.)

**Duplicate collapse is last-wins everywhere.** An extractor
emitting two entries with the same encoded key in one
invocation collapses to the LAST entry (set semantic) on the
live maintenance path, during `Rebuild`, during `BulkLoad`
(which reuses the live path's extraction), and in the checker's
expected-set construction — a rebuilt index is byte-identical
to a live-maintained one. (For unique indexes the duplicate is
an `ErrIndexUniqueViolation` candidate-set collision instead.)
Pinned by TestRebuildMatchesLiveDuplicateCollapse.

**Byte-API return contract.** For the byte-oriented `*IndexHandle`
surface, the bytes `Lookup` / `Range` / `Prefix` / `Get` yield
as the `value` ARE the encoded covering tuple stored by the
engine — the NUL-escape multi-column blob produced from
`IndexEntry.Cover`. The caller recovers the per-column
`[][]byte` via `DecodeCoveringTuple(value)`. This is symmetric
with the storage encoder (`encodeIndexKey`) reused for covering
bytes per the grammar above. An index whose `Covering` is empty
continues to back-lookup the row keyspace and returns the row's
stored bytes — the contract switches on whether the
`IndexDecl.Covering` slice is non-empty, not on any runtime
flag.

A covering tuple with zero entries (extractor returned
`Cover=nil` despite a non-empty `Covering` declaration) is
stored as empty bytes and `Lookup` returns an empty `value`;
the engine permits this case to keep the storage / read paths
total. The extractor contract is "one `Cover[i]` per declared
`IndexCoveringColumn`" — producing fewer is a caller-side contract
violation, not an engine error.

The typed full-row covering helper (`TypedIndex.CoverValue`,
see `typed-keyspaces.md §Covering`) is the typed-layer
specialization: its extractor stores `encode(V)` as the single
covering column, and `TypedKeyspaceHandle.Index` enables an internal
single-column unwrap so `TypedIndexQuery.Lookup` returns `V`
without forcing the caller to call `DecodeCoveringTuple`
themselves.

**Names are semantic anchors, not positional labels.** Covering
and indexed column names are inputs to the schema hash
specifically to catch *structural* changes. They do not catch
the case where a caller reuses the same name for a column whose
meaning has changed (e.g. renaming `"price"` to `"qty"` and
`"qty"` to `"price"`, then populating each with the other's
value — schema hash unchanged, stored entries silently decode
into the wrong logical columns). That case requires bumping
`Version` — the `Version` tag exists precisely to catch
logic-level drift the engine cannot see.

## Partial Indexes

The extractor returns an empty `[]IndexEntry` for rows that
should not be indexed. The engine does not write any entries
for those rows. On Update, the old and new entry sets are
diffed: an entry present in the old set but absent from the new
is deleted; one present in the new but absent from the old is
inserted.

There is no separate "predicate" primitive — the extractor *is*
the predicate. Simpler API, equivalent expressive power.

## Lookup API

Index queries are exposed on a `*IndexHandle` handle returned by
`Keyspace.Index(name)` or `SetKeyspace.Index(name)`. The
canonical query is `Lookup`; `LookupKeys` is the cost-sensitive
escape hatch.

For the Go-level signatures, see `api-surface.md §Index Lookup
API`. Brief summary:

- `Lookup(cols...)` → `iter.Seq2[pk, value]` — exact match on
  all declared columns. `value` is read from the index entry's
  covering bytes when the index covers the requested columns;
  otherwise via back-lookup against the row keyspace.
- `LookupKeys(cols...)` → `iter.Seq[pk]` — same matching, no
  back-lookup, no covering decode.
- `Range(start, end [][]byte)` → `iter.Seq2[pk, value]` —
  matches in `[start, end)`. `nil` tuple ⇒ open-ended.
- `Prefix(leadingCols...)` → `iter.Seq2[pk, value]` — matches
  whose leading columns equal the prefix.
- `Get(cols...)` — shorthand for unique indexes: returns the
  single `(pk, value)` or `ErrNotFound`; returns
  `ErrIndexNotUnique` on a non-unique index.
- `Err()` returns the first error encountered during the last
  sequence's iteration. The `Err` state is per-handle; two
  overlapping iterators on the same `*IndexHandle` race — open the
  keyspace in separate transactions, or call
  `ks.Index(name)` once per goroutine.

**Intra-transaction consistency.** Index cursor and back-lookup
both read the current transaction's dirty state. Row writes
and index updates happen atomically in the same `Put` / `Delete`
/ `Cursor.Delete`, so a back-lookup for an index entry always
finds the row. If a back-lookup ever fails to find its PK
(engine bug or external corruption), the entry is silently
skipped from `Lookup`'s iteration and the inconsistency is
reportable via `Check()`. Two surfaces do **not** probe the row
keyspace and therefore do **not** observe the silent-skip case:
`LookupKeys` (raw PK only) and covering-`Lookup` / `Range` /
`Prefix` / `Get` on an index declaring `Covering` (the value
comes from the index entry itself — see §Byte-API return
contract above). On those surfaces an index-versus-row
inconsistency surfaces only via `Check()`.

### Handle Invalidation

An `*IndexHandle` handle returned by `ks.Index(name)` is bound to the
parent keyspace for the lifetime of the transaction. Mutations
that replace or free the index's data tree pages within the same
transaction invalidate in-flight observers tied to that handle.
Four distinct invalidation conditions, each with its canonical
sentinel — identical to the row-cursor contract that
`transactions.md §Cursor State Machine` defines:

- **`ErrCursorStale` (mid-iter cursor invalidation).** Triggered by
  `tx.Indexes().Rebuild(ks, decl)` for the named index, `tx.Indexes().Drop`
  for the named index, or any successful mutation of the parent
  indexed keyspace that runs the atomic index-maintenance step:
  Keyspace `Put` / `Delete` / `DeleteRange` (indexed-fallback —
  routes through per-row `Cursor.Delete`) / `Cursor.Delete`; and
  the SetKeyspace mutators `Put` / `Delete` / `DeleteValue` /
  `DeleteRange` (cursor-walk) / `SetCursor.Delete` (which delegates
  to `DeleteValue`). The next `c.Next()` inside a `Lookup` /
  `LookupKeys` / `Range` / `Prefix` iter closure surfaces
  `ErrCursorStale` (via the in-iter `*btree.Cursor.MarkStale`
  machinery); iteration terminates and `idx.Err()` reports
  `ErrCursorStale`. The caller's recovery is to re-iterate via a
  fresh `idx.Lookup` / `idx.Range` / `idx.Prefix` — the new iter
  opens a fresh cursor on the current `idx.pinned.root`, descending
  from the live (post-mutation) tree. If `RebuildIndex` changed the
  index's column shape (a different `IndexDecl.Columns` slice), the
  cached handle's re-`Lookup` with the OLD shape returns
  `ErrInvalidOptions` (`got N cols, want M`); full recovery from a
  shape-changing rebuild is to re-`OpenKeyspace` with the new
  `IndexDecl` and obtain a fresh `*IndexHandle` via `ks.Index(name)`.
  `BulkLoad` is invalidation-irrelevant here: its precondition
  (`Count == 0`) makes any in-flight iter cursor unreachable (the
  iter closures early-return at `pinned.root == 0`).

- **`ErrIndexNotFound` (post-Drop dead-handle).** Triggered by
  `tx.Indexes().Drop(ks, name)` on the handle's index. The handle
  transitions to "dead": every subsequent
  `Lookup` / `LookupKeys` / `Range` / `Prefix` / `Get` / `Stats`
  call sets `idx.Err()` (or returns directly, for `Get` / `Stats`)
  to a wrapped `ErrIndexNotFound` — the same sentinel
  `ks.Index(name)` returns for a name that does not exist. Dead is
  permanent within the transaction; a re-`Index(name)` after the
  drop returns `ErrIndexNotFound` for the same reason.

- **`ErrKeyspaceClosed` (post-DeleteKeyspace handle closed).**
  Triggered by `tx.DeleteKeyspace(ks.Name())` on the handle's
  parent keyspace. `retireIndexRegistry` walks every declared
  index's `Root` and `FreeSubtree`s the data tree, and the cached
  handle's `idx.pinned.root` is now a freed page. Every subsequent
  `Lookup` / `LookupKeys` / `Range` / `Prefix` / `Get` / `Stats`
  call sets `idx.Err()` (or returns directly, for `Get` / `Stats`)
  to `ErrKeyspaceClosed` — distinct from the post-Drop dead-handle
  sentinel because the WHOLE keyspace is gone, not just this
  index. Closes the
  `transactions.md §Cursor invalidation by DeleteKeyspace` clause
  for `*IndexHandle` handles (the clause names them but the row-cursor
  fix that landed at chunk 5.6 did not enforce it on the iter
  surface). Dead-check ordering is "parent first":
  `Stats` / `Lookup` / etc. probe the parent ks/sks `dead` flag
  before the handle's own `dead`, so a handle whose index was
  dropped AND whose keyspace was then deleted in the same tx
  reports `ErrKeyspaceClosed` (the broader truth) rather than
  `ErrIndexNotFound` (the narrower one) — matching `Cursor.Err`'s
  dead-check-wins ordering. The mid-iter case (a
  `tx.DeleteKeyspace` call from inside a user `for-range` over
  `idx.Lookup`) MarkStales the in-flight `*btree.Cursor` via
  `markIndexHandlesStale` during `tx.DeleteKeyspace`'s in-memory
  invalidation block, and the closure's err translation maps the
  resulting `btree.ErrCursorStale` to `ErrKeyspaceClosed`
  (`mapCursorErr` checks `keyspaceDead()` before returning
  `ErrCursorStale` — the "re-position to recover" semantic of
  `ErrCursorStale` does not apply when the parent is gone).

- **`ErrTxClosed` / `ErrChildActive` (owning transaction not open).**
  Every query surface — `Lookup` / `LookupKeys` / `Range` / `Prefix`
  / `Get` / `Stats` — probes the owning transaction's state
  (`requireOpen`) after the dead-keyspace and dead-handle checks, and
  the bare `Err()` poll probes it right after its dead-keyspace check
  (before the sticky Inv-IHS1 cause — an unrecoverable lifecycle fact
  wins over a re-iterable one). A handle whose transaction has closed
  rejects with `ErrTxClosed`; a handle whose transaction is frozen by
  an active child (the parent-freeze of `transactions.md §Nested
  Transactions`) rejects with `ErrChildActive`. This is the general
  handle-lifetime contract applied to `*IndexHandle`, and it covers a
  handle obtained from a child transaction's OWN `OpenKeyspace`: once
  the child commits or rolls back, the handle errors `ErrTxClosed`,
  honoring the `transactions.md §Nested Transactions` **Handle
  lifetime** clause ("every child handle returns `ErrTxClosed` once
  the child commits or rolls back"). The post-rollback case is
  load-bearing — the child's savepoint-reverted index pages must never
  be descended: a freed page that re-parses as a leaf yields silently
  wrong data, one that parses as a non-leaf yields `ErrCorrupted`. The
  probe runs AFTER the dead-keyspace / dead-handle checks so the more
  specific `ErrKeyspaceClosed` / `ErrIndexNotFound` sentinels win when
  both apply.

Five invariants pin this contract (Inv-IHS4 / Inv-IHS5 below the
enforcement note):

- **Inv-IHS1 (cursor-on-stale-tree).** A `*btree.Cursor` opened by
  an `*IndexHandle` iter closure is `MarkStale`'d (and its tracked rootID
  refreshed to `idx.pinned.root`) before any same-tx code path
  completes that frees or replaces the index data tree pages it
  walks. Violation: an iter's `c.Next()` reads CoW'd-then-released
  or `FreeSubtree`'d-then-reallocated leaf pages → wrong-key
  yields or layout-decode panics.

- **Inv-IHS2 (post-drop handle dead).** After `tx.Indexes().Drop(ks,
  name)` succeeds, every previously-handed-out `*IndexHandle` handle for
  the `(ks, name)` pair rejects subsequent
  `Lookup` / `LookupKeys` / `Range` / `Prefix` / `Get` / `Stats`
  with `ErrIndexNotFound`. Violation: `idx.Stats()` returns the
  pre-Drop `Count` and `idx.Lookup()` walks freed root pages.

- **Inv-IHS3 (post-DeleteKeyspace handle closed).** After
  `tx.DeleteKeyspace(ks.Name())` succeeds, every previously-
  handed-out `*IndexHandle` handle whose parent is `ks` rejects
  subsequent `Lookup` / `LookupKeys` / `Range` / `Prefix` / `Get`
  / `Stats` / `Err` with `ErrKeyspaceClosed` — checked BEFORE the
  Inv-IHS2 `idx.dead` check, so the broader sentinel wins on a
  drop-then-delete sequence. `Err()`'s `keyspaceDead`-first
  ordering keeps the bare-`Err()` poll observation symmetric with
  `Stats` / `Lookup` / etc. — a user polling `Err()` after
  `tx.DeleteKeyspace` (regardless of any prior bad-cols Lookup or
  mid-iter sibling-mutation stale that may have left a sticky
  `idx.err`) observes `ErrKeyspaceClosed`, matching what
  `Stats()` / `Lookup()` report for the same state. The iter-side
  sticky `idx.err` (Inv-IHS1) remains observable only on a live
  keyspace; on a dead keyspace, the broader truth supersedes it.
  Any `*btree.Cursor` opened by an
  in-flight `idx` iter closure is `MarkStale`'d before the
  surrounding `tx.DeleteKeyspace` call returns (via
  `markIndexHandlesStale` in the in-memory invalidation block),
  and the closure's `idx.mapCursorErr` translates the resulting
  `btree.ErrCursorStale` to `ErrKeyspaceClosed` because
  `keyspaceDead()` is now true. Violation: `idx.Stats()` returns
  the pre-Delete `Count`, `idx.Lookup()` walks the freed root, or
  a mid-iter `tx.DeleteKeyspace` from inside a user `for-range`
  yields stale entries from the just-`FreeSubtree`'d pages with
  no MarkStale signal. Closes the
  `transactions.md §Cursor invalidation by DeleteKeyspace` clause
  for `*IndexHandle` handles (the clause names them but the row-cursor
  fix that landed at chunk 5.6 did not enforce it on the iter
  surface).

Per the chunk-5.6 row-cursor invalidation pattern these
invariants are spec-tier *and* enforced (regression tests
`TestIndexHandleStatsAfterDropReturnsErrIndexNotFound`,
`TestIndexHandleStatsAfterDeleteKeyspaceReturnsErrKeyspaceClosed`,
`TestIndexHandleInFlightDeleteKeyspaceSurfacesErrKeyspaceClosed`
et al. on the package, plus the markup of the contract on this
section).

- **Inv-IHS4 (child-commit handle reconciliation).** When a child
  transaction commits, every `*IndexHandle` the PARENT handed out
  before `BeginChild` is re-pointed at the merged (child-committed)
  index state: lookups and `Stats` reflect the child's work exactly
  as a freshly-opened handle would; a child `Drop` leaves the
  parent's handle dead per Inv-IHS2 (the child freed the subtree —
  the handle must never descend it); in-flight cursors are
  stale-marked with the merged root. Violation: the parent handle
  keeps serving the pre-child tree — silently wrong results with
  `Err() == nil` (the audit demonstrated 1 row via the stale handle
  vs 201 via a fresh one), or a freed-page descent after a child
  Drop. (Enforced by reconcileIndexHandles in the child-commit
  merges; pinned by TestIndexHandleSeesChildCommit /
  TestIndexHandleDeadAfterChildDrop /
  TestSetIndexHandleSeesChildCommit.)

- **Inv-IHS5 (owning-tx not open).** Every `*IndexHandle` query
  surface (`Lookup` / `LookupKeys` / `Range` / `Prefix` / `Get` /
  `Stats`) and the bare `Err()` poll reject with `ErrTxClosed` once
  the owning transaction has closed, and with `ErrChildActive` while
  the owning transaction is frozen by an active child. The
  load-bearing case: a handle obtained from a child transaction's own
  `OpenKeyspace` errors `ErrTxClosed` the instant the child commits or
  rolls back — never serving the child's now-merged rows (post-commit)
  and never descending its savepoint-reverted index pages
  (post-rollback). Ordering differs by surface: the six query
  surfaces probe AFTER both the Inv-IHS3 keyspace-dead and the
  Inv-IHS2 dead-handle checks, so `ErrKeyspaceClosed` /
  `ErrIndexNotFound` win when they apply; the bare `Err()` poll probes
  after the keyspace-dead check but BEFORE the dead-handle check — its
  documented broadest-truth-first ordering, where an unrecoverable
  lifecycle fact wins over the re-`Index`-recoverable dead-handle
  sentinel (extending the Inv-IHS2 Err-vs-`Stats` residual asymmetry).
  So a handle Dropped inside a child and then observed after the child
  resolves reports `ErrIndexNotFound` from `Stats` but `ErrTxClosed`
  from a bare `Err()` — each correct for its surface.
  Violation: a child-created handle's `Lookup` keeps yielding rows
  with `Err() == nil` after `child.Commit()`, or descends
  `FreeSubtree`'d pages after `child.Rollback()` (`ErrCorrupted`, or
  silently wrong data if a freed page re-parses as a valid leaf).
  (Enforced by TestChildIndexHandleErrsAfterChildCommit /
  TestChildIndexHandleErrsAfterChildRollback — every surface plus the
  bare `Err()` poll reports `ErrTxClosed` — and
  TestParentIndexHandleFrozenByActiveChild for the `ErrChildActive`
  arm.)

## Write Path: Atomic Index Maintenance

For an indexed keyspace, every `Put`, `Delete`, and
`Cursor.Delete` operation is wrapped:

**Put(key, newValue):**

1. Read the existing value at `key` (if present), call it
   `oldValue`.
2. Call `extract(key, oldValue)` → `oldEntries` (empty list if
   no existing row).
3. Call `extract(key, newValue)` → `newEntries`.
4. Diff `oldEntries` and `newEntries`: deletes (in old, not in
   new), inserts (in new, not in old).
5. For each unique-index insert, probe the index for an existing
   entry; conflict ⇒ return `ErrIndexUniqueViolation` (no row
   write, no index write).
6. Apply index deletes.
7. Apply index inserts (each writes to the index's internal
   keyspace).
8. Write the row to the main keyspace.
9. Update each index's `Count` in the registry.

All steps happen in the same CoW transaction. A failure at any
step (including the unique probe) leaves the transaction in a
consistent state — either rolled back, or continuing with the
row unchanged.

**Delete(key):**

1. Read the existing value at `key` (if present). Absent ⇒
   no-op.
2. Call `extract(key, oldValue)` → `oldEntries`.
3. Delete all entries in `oldEntries` from their indexes.
4. Decrement each affected index's `Count`.
5. Delete the row.

**Cursor.Delete():** same as `Delete` but uses the cursor's
current key/value (already in hand).

## Bulk Operations on Indexed Keyspaces

`DeleteRange(start, end)` on an indexed keyspace **does not**
use the O(pages) subtree-retirement fast path of
`range-delete.md`. The engine cannot retire a subtree without
knowing the prior-index-keys for every row in it (the extractor
output depends on the row's value, which the subtree-retirement
walk does not visit).

Implementation: the engine iterates the range with a cursor,
calling `Delete()` for each row. Cost is
`O(entries × (indexes + extractor))`. The cursor must remain
stable across CoW + rebalance triggered by the per-row deletes —
the canonical drain loop reads the post-delete successor via
`Cursor.Current()` (NOT `Cursor.Next()`, which would skip,
since `Cursor.Delete()` already advances). See
`transactions.md §Cursor State Machine` for the full pattern.

This is the same cost a SQL engine pays for `DELETE … WHERE …
IN range` with secondary indexes. Predictable and correct.

Callers needing the O(pages) fast path on indexed data can:

- Drop the indexes before the bulk operation, run
  `DeleteRange`, then rebuild the indexes (`tx.Indexes().Rebuild`).
- Or use `DeleteKeyspace` to drop the whole keyspace (which
  also drops its indexes — the engine cleans up internal
  index keyspaces and the per-keyspace index registry).

## Rebuild

`tx.Indexes().Rebuild(keyspace, decl)` drops the named index's data
and re-runs the extractor supplied in `decl` over every row in
the keyspace, writing fresh index entries. Blocking — runs
inside the current write transaction. The previous index is
preserved until commit; mid-rebuild crash leaves the old index
intact.

`decl.Name` must match the name of an index already declared
on the keyspace (the registry entry's stored Name). The
supplied decl replaces the stored `SchemaHash` and `Version`
on success; this is the canonical recovery path after
`ErrIndexFingerprintMismatch` because the rebuild bypasses the
open-time fingerprint check.

The keyspace itself is opened internally for cursor iteration
without re-validating other indexes' fingerprints. If the
same transaction also needs to open the keyspace for writes,
it must supply matching IndexDecls for every still-drifted
index — or call `RebuildIndex` once per drifted index before
calling `OpenKeyspace`.

`decl.Extract` MUST be non-nil; a nil `Extract` returns
`ErrIndexExtractorRequired`.

### Recovery pattern after `ErrIndexFingerprintMismatch`

A single `OpenKeyspace` call reports drift on *one* index at a
time (the first mismatch encountered while iterating the
declared set). When multiple indexes have drifted
simultaneously — common during a schema-bumping deploy — the
recovery requires a loop: rebuild the named index, retry the
open, rebuild whichever index the *next* mismatch names, retry,
until `OpenKeyspace` succeeds. The decl set passed to
`OpenKeyspace` stays constant; only the `RebuildIndex` calls
fire in succession:

```go
// defer tx.Rollback() — partial rebuilds discard cleanly if the
// loop exits with an error.
decls := []*IndexDecl{byRepoDecl, activeLeaseDecl}
for {
    ks, err = tx.OpenKeyspace("workspaces", decls...)
    if err == nil {
        break
    }
    var fpErr *IndexFingerprintError
    if !errors.As(err, &fpErr) {
        return err // some other error — propagate
    }
    var d *IndexDecl
    for _, candidate := range decls {
        if candidate.Name == fpErr.IndexName {
            d = candidate
            break
        }
    }
    if d == nil {
        return fmt.Errorf("drifted index %q not in supplied decls", fpErr.IndexName)
    }
    if err := tx.Indexes().Rebuild("workspaces", d); err != nil {
        return err
    }
}
// ks is now usable.
```

### Recovery on `RebuildIndex` failure

A `RebuildIndex` call that returns an error leaves the
transaction in a partially-rebuilt state — earlier indexes in
the loop iteration may already have their new
`SchemaHash`/`Version` staged for commit, while the failing
index was rolled back to its prior state. The transaction is
**not** safe to commit in that state; the caller must
`tx.Rollback()` (the `defer` above) and start a fresh
transaction. Specifically:

- `ErrTxTooLarge` from `RebuildIndex` means the keyspace's row
  corpus exceeds `MaxTxBufferBytes` for a single rebuild. Use
  `BulkLoad` (which bypasses the slab) into a fresh keyspace,
  or chunk the rebuild manually across multiple write
  transactions using a shadow-index + cutover pattern.
- `ErrIndexUniqueViolation` means the new extractor produced
  duplicate keys that the unique constraint rejected. The
  extractor logic is wrong (or the partial-index predicate is
  wrong); rollback, fix the extractor in source, redeploy,
  retry.
- Any other error (I/O, `ErrDBFull`, etc.) is a hard failure;
  rollback and surface upstream.

A degenerate-but-safe simplification for callers that don't
care about per-index reporting is to call `RebuildIndex` for
*every* declared index unconditionally on first mismatch — at
the cost of rebuilding indexes that may not have drifted.

### Rebuild mechanics

1. Allocate a new internal index keyspace (fresh root page).
2. Cursor-iterate the parent keyspace. The internal cursor
   sees the current write transaction's dirty state — rows
   `Put` earlier in the same transaction are included in the
   rebuilt index. For each row, run the extractor from `decl`
   and write entries into the new index keyspace. For unique
   indexes, any extractor-produced duplicate aborts the
   rebuild with `ErrIndexUniqueViolation` — the rebuild does
   not commit and the existing registry entry is unchanged.
3. Update the registry entry: new `Root`, new `Count`, new
   `SchemaHash` (computed from `decl`), new `UserVersion`
   (from `decl.Version`). The old internal index keyspace's
   pages enter `tx.retiredPages`.
4. On `tx.Commit()`, the new index becomes active; old pages
   reclaim via the RPL.

`Index.Stats()` called on a handle to the still-rebuilding
index returns the *old* registry entry's count and tree
statistics until the transaction commits — the new index is
invisible until the registry write in step 3 lands at commit.
A caller calling `Stats()` mid-`RebuildIndex` therefore sees
the pre-rebuild state, not an intermediate.

For very large keyspaces this may exceed `MaxTxBufferBytes` —
the rebuild fails with `ErrTxTooLarge` and the caller must use
`BulkLoad` instead (see `bulkload.md §Interaction with
Indexes`), or chunk the rebuild manually.

## Indexes on SetKeyspaces

A SetKeyspace can carry indexes. The extractor signature is the
same `func(key, value []byte) []IndexEntry`, but it runs
**per (key, value) set member**, not per top-level key. The
"primary key" in non-unique index entries is the `(key, value)`
pair — neither alone identifies the set member.

For the compound-PK encoding (separator `0x00 0x01`), see
`set-keyspace.md §Indexes on SetKeyspaces`.

`Cursor.Delete()` on a set keyspace deletes one set member;
index updates affect only that member's contribution.
`Delete(key)` on a set keyspace removes all members; index
updates run the extractor on each removed `(key, value)` pair.

Bulk-free of a key's nested B+tree (via `Delete(key)`) reverts
to a per-member walk when the SetKeyspace has indexes — same
reasoning as `DeleteRange` on indexed keyspaces.

## Open Semantics

Two distinct open functions:

- `OpenKeyspace(name, ...IndexDecl)` opens a keyspace for
  read+write. Requires every declared index on the keyspace to
  be supplied with a matching `IndexDecl`. Missing or extra
  IndexDecls return `ErrIndexExtractorRequired` or
  `ErrIndexUnknown`. Drift returns
  `ErrIndexFingerprintMismatch` (caller must `RebuildIndex`).
- `OpenKeyspaceReadOnly(name)` opens a keyspace for reads only.
  No IndexDecls required (and none accepted — pass them via
  `OpenKeyspace` if you want write access). Index lookups
  still work — they read stored index entries directly.

Strict — opening for write without the extractors is
unrepresentable. Two open functions instead of "open succeeds,
writes error" because the failure-at-open path:

- Surfaces drift / missing extractors immediately, before any
  work.
- Lets backup/inspector/read-only tools open without schema
  awareness, using `OpenKeyspaceReadOnly`.
- Avoids the "open succeeded, but every subsequent write
  fails" state that's easy to miss in operational settings.

`OpenSetKeyspace` / `OpenSetKeyspaceReadOnly` follow the same
pattern.

A keyspace handle returned from `OpenKeyspaceReadOnly` rejects
all mutating operations with `ErrReadOnly`. Index lookups,
cursor reads, and range iteration work normally.

### Re-opening a keyspace in the same transaction

A second `OpenKeyspace` call for the same name within one
transaction:

- If the supplied `IndexDecl` set is identical to the first
  call's set by **all hashable inputs** — names, `Unique`
  flags, schema hashes, `Version`s, and (for typed indexes)
  encoder IDs — returns the *same* `*Keyspace` handle
  (idempotent).
- If the supplied `IndexDecl` set differs by any hashable
  input — even by one decl — returns `ErrKeyspaceAlreadyOpen`
  with the conflicting index name(s). Indexes declared on a
  keyspace are pinned for the lifetime of the transaction at
  first open.

**First-Extract-wins.** Go function values are not comparable,
so the `Extract` function pointer is NOT part of the
hashable-inputs comparison. Two `OpenKeyspace` calls with
structurally identical IndexDecls but **different** `Extract`
functions are treated as identical: the first call's
`Extract` is registered and wins for all subsequent index
maintenance within the transaction; the second call's
`Extract` is silently dropped.

The two callers receive the *same* `*Keyspace` handle by
design (idempotent re-open), so writes from either caller
through that shared handle go through the first-registered
`Extract`. If both goroutines legitimately want distinct
extractor behaviours, the only correct pattern is **separate
transactions** — there is no in-transaction recovery path
because index maintenance is pinned at first open. Forcing
recognition via a hashable input (typically bumping
`Version`) is *not* recovery: it converts the second call to
`ErrKeyspaceAlreadyOpen` (the schema-hash now differs), which
also doesn't yield a working second handle in the same txn.

Mixing `OpenKeyspace` and `OpenKeyspaceReadOnly` for the same
name in one transaction is also rejected with
`ErrKeyspaceAlreadyOpen`. Rationale: the read-only handle and
the read-write handle have different operational contracts
(Extractors required vs forbidden; Put/Delete allowed vs
`ErrReadOnly`), and pinning one shape per transaction keeps
the per-keyspace open-registry invariants simple. Callers
needing both shapes use separate transactions.

## Removing an Index

`tx.Indexes().Drop(keyspace, indexName)` removes the index entry
from the per-keyspace registry and retires the index's
internal keyspace pages. Future `OpenKeyspace` calls must
omit the corresponding `IndexDecl`, or a fresh declaration
with the same name re-creates the index empty (next `Put`
populates it as rows are written; existing rows are NOT
auto-indexed — call `RebuildIndex` if you want existing rows
indexed).

`Drop` (and `Rebuild`) work whether or not the keyspace is
currently open in the transaction, and the outcome is
order-independent within the transaction: a subsequent same-tx
open of the keyspace — any variant, including read-only —
observes the post-Drop registry, and the drop persists at
commit regardless of intervening opens. The same holds for
`Tx.SetKeyspaceConfig` (`keyspaces.md §Per-Keyspace
Configuration`): a descriptor mutation staged against a
not-yet-open keyspace is never silently discarded by a later
open of that keyspace in the same transaction.

## Statistics

`Index.Stats()` returns the index's persistent count + B+tree
statistics (depth, pages). Iteration via `Lookup` does not
count under-the-hood pages read; that comes from
`Tx.Stats()`.

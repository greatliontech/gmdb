# Indexing

gmdb maintains secondary indexes on keyspaces declaratively. The
caller declares one or more indexes per keyspace at open time,
supplying an extractor function that produces index entries from a
row. The engine applies index changes inside every write
transaction that modifies the keyspace, atomic with the row write.

Scope:
- `IndexDecl`, `IndexColumn`, `CoveringColumn`, `IndexEntry`,
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
    Covering []CoveringColumn   // optional; stored in the index value
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

type CoveringColumn struct {
    Name string // same semantics as IndexColumn.Name
}

type IndexEntry struct {
    Cols  [][]byte // one byte slice per IndexColumn; lex-safe encoded by caller
    Cover [][]byte // one per CoveringColumn (omit when Covering is nil)
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
  index.Name ||
  uvarint(len(Columns)) || for each col: uvarint(len(Name)) || Name ||
  uvarint(len(Covering)) || for each col: uvarint(len(Name)) || Name ||
  uint8(Unique)
)
```

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

The caller's recovery path is `tx.RebuildIndex` — see Rebuild
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

Index queries are exposed on a `*Index` handle returned by
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
  overlapping iterators on the same `*Index` race — open the
  keyspace in separate transactions, or call
  `ks.Index(name)` once per goroutine.

**Intra-transaction consistency.** Index cursor and back-lookup
both read the current transaction's dirty state. Row writes
and index updates happen atomically in the same `Put` / `Delete`
/ `Cursor.Delete`, so a back-lookup for an index entry always
finds the row. If a back-lookup ever fails to find its PK
(engine bug or external corruption), the entry is silently
skipped from `Lookup`'s iteration and the inconsistency is
reportable via `Check()`. `LookupKeys` does not probe and
therefore does not observe the silent-skip case.

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
stable across CoW + rebalance triggered by the per-row deletes
— `Cursor.Delete()` followed by `Cursor.Next()` is defined to
correctly resume at the post-delete successor (see
`transactions.md §Cursor State Machine`).

This is the same cost a SQL engine pays for `DELETE … WHERE …
IN range` with secondary indexes. Predictable and correct.

Callers needing the O(pages) fast path on indexed data can:

- Drop the indexes before the bulk operation, run
  `DeleteRange`, then rebuild the indexes (`tx.RebuildIndex`).
- Or use `DeleteKeyspace` to drop the whole keyspace (which
  also drops its indexes — the engine cleans up internal
  index keyspaces and the per-keyspace index registry).

## Rebuild

`tx.RebuildIndex(keyspace, decl)` drops the named index's data
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
    if err := tx.RebuildIndex("workspaces", d); err != nil {
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

`tx.DropIndex(keyspace, indexName)` removes the index entry
from the per-keyspace registry and retires the index's
internal keyspace pages. Future `OpenKeyspace` calls must
omit the corresponding `IndexDecl`, or a fresh declaration
with the same name re-creates the index empty (next `Put`
populates it as rows are written; existing rows are NOT
auto-indexed — call `RebuildIndex` if you want existing rows
indexed).

## Statistics

`Index.Stats()` returns the index's persistent count + B+tree
statistics (depth, pages). Iteration via `Lookup` does not
count under-the-hood pages read; that comes from
`Tx.Stats()`.

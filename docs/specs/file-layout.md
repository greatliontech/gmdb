# File Layout

Defines the on-disk macro-layout of the database file: regions, page
size constraints, and the meta-page format. This spec is the
authoritative source for which file offsets contain what; per-page
formats for tree and value storage live in `page-formats.md`, and
free-space tracking pages live in `free-space.md`.

Scope:
- File regions (meta pages, bitmap region, data pages).
- Common page header.
- Meta page format, fields, and atomicity rules.

Depends on / interacts with:
- `checksums.md` — meta page checksum and optional data-page
  footers, including the meta-page `Flags` bit assignments.
- `free-space.md` — bitmap region size derivation and references
  from meta to bitmap/RPL.
- `pager-slab.md` — meta-page write protocol at commit time.
- `file-format.md` — `MinSize` / `MaxSize` / grow / shrink rules.

## Invariants

Invariant: kind=clause-explicit;
  property=Every multi-byte integer in the data file is stored in
    little-endian byte order;
  from=this spec §Byte order;
  violation=A big-endian-built reader of a little-endian-written file
    decodes wrong page IDs and field values, silently traversing into
    the wrong region.

Invariant: kind=clause-explicit;
  property=`PageSize` is a power of two in [4096, 65536], set at
    creation, persisted in the meta page, and immutable afterwards;
  from=this spec §Meta Page (`PageSize`);
  violation=Re-opening a database with a different effective
    `PageSize` misaligns every page offset (offset = pageID × pageSize)
    and corrupts every read.

Invariant: kind=clause-explicit;
  property=Pages 0 and 1 are the two meta pages; pages 2 through
    `2 + BitmapPages - 1` are the bitmap region; data pages begin at
    `2 + BitmapPages`. `BitmapPages` is fixed at creation and stored
    on the meta page;
  from=this spec §File regions;
  violation=Treating bitmap-region pages as data pages (or vice
    versa) corrupts the allocation bitmap on bitmap-pwrite and breaks
    every subsequent allocation.

Invariant: kind=clause-explicit;
  property=A page's ID is implicit, computable from its file offset
    as `offset / PageSize`. No stored PageID exists or is checked;
  from=this spec §Page Header;
  violation=Storing a PageID that disagrees with file position is the
    failure mode this design avoids; if implementers add a redundant
    PageID field, recovery cannot pick a single source of truth.

Invariant: kind=entailed;
  property=Of the two meta pages, the **active** meta is the one with
    the highest `TxnID` whose xxhash64 checksum is valid (with the
    checkpoint-flag precedence rule in `durability.md`); the other
    meta points to a strictly older consistent state;
  from=entailed: dual-meta + atomic-swap commit (this spec + 12);
  violation=A reader/writer that picks the wrong meta on Open sees a
    tree that is either stale (silent rollback) or partially-published
    (torn write — caught by checksum, but only if the rule is honoured).

Invariant: kind=entailed;
  property=A successful commit writes a strictly-greater `TxnID` than
    the previously-active meta's `TxnID`. `TxnID` rises monotonically by
    at least one per committed transaction;
  from=entailed: meta active-selection rule (preceding invariant) plus
    `pager-slab.md §Commit Write Ordering` — selection is unambiguous
    only when the two metas carry distinct `TxnID`s after the first
    commit;
  violation=Two metas with equal non-zero `TxnID`s leave active-meta
    selection undefined; a reader can observe a previous tree the
    writer thought it had superseded, or vice-versa. The dual-meta
    swap protocol is the only path that writes a meta, so any equal-
    `TxnID` pair on disk after the first commit is a commit-protocol
    bug.

Invariant: kind=entailed;
  property=`HighWaterMark` in the meta page is monotonically
    non-decreasing within any single recoverable commit chain (it
    only retreats when a successful tail-page-refund commit lowers it
    via the same commit-swap protocol that publishes any other meta
    field);
  from=entailed: tail refund + meta swap atomicity (this spec + 05 +
    06);
  violation=A reader observing a HighWaterMark larger than the active
    meta promises can SIGBUS on the unmapped beyond-file region; a
    HighWaterMark that retreats without a commit chain protecting it
    exposes pages a still-active reader holds.

Invariant: kind=clause-explicit;
  property=`Open()` rejects a database whose meta has any unknown
    `Flags` bit set. Bit 0 (PageChecksum) is immutable; bit 1
    (Checkpoint) is mutable;
  from=this spec §Meta Page;
  violation=A future engine version that introduces a new flag bit
    silently misinterprets pages written by an older engine that did
    not understand it — forward-compat trap.

## File regions

A database is a single file divided into fixed-size pages. All pages
in a database are the same size (configurable at creation, immutable
after). Supported page sizes are powers of 2 from 4 KB to 64 KB.
Default: 4096 bytes.

```
+--------+--------+------------------+--------+--------+----
| Meta 0 | Meta 1 | Bitmap Pages ... | Data pages ...       |
| Page 0 | Page 1 | Page 2 .. N      | Page N+1, N+2, ...   |
+--------+--------+------------------+--------+--------+----
```

Bitmap pages occupy a contiguous region starting at page 2. The
number of bitmap pages is determined by `MaxSize` at database
creation time:

```
BitmapPages = ceil((MaxSize / PageSize) / (PageSize * 8))
```

Data pages (B+tree nodes, overflow pages, RPL segment pages) begin
immediately after the bitmap region. See `free-space.md` for the
bitmap and RPL details.

## Page Header

Every page except meta pages and bitmap pages starts with a common
8-byte header:

```
Page Header (8 bytes)
+----------+----------+----------+-----------------+
| Type     | Flags    | Count    | AdditionalPages |
| uint8    | uint8    | uint16   | uint32          |
+----------+----------+----------+-----------------+
```

- **Type** (uint8): one of `Branch`, `Leaf`, `Overflow`,
  `RPLSegment`, `LeafUncompressed`. `Leaf` is the
  prefix-compressed leaf variant; `LeafUncompressed` is the
  variant selected by `RestartGroupTarget == 1` (see
  `page-formats.md §Leaf Page`). Meta pages and bitmap pages do
  not carry the page header.
- **Flags** (uint8): reserved for future per-page flags. Must be
  zero on write. Readers must reject pages with unknown flags set.
- **Count** (uint16): number of items (cell count for branch / leaf,
  entry count for RPL segment).
- **AdditionalPages** (uint32): number of contiguous overflow pages
  following this one (0 for single-page nodes).

### Reserved-byte policy (project-wide)

Three distinct categories of "reserved" bytes appear across
on-disk structures, with different read-time rules:

- **Reserved flag bits** (page header `Flags`, leaf `CellFlags`).
  *Strict-reject* on read: a page with unknown flag bits set is
  rejected as structural corruption. This catches accidental
  cross-format reads and forbids silent semantic drift if a
  future writer sets a bit a reader doesn't understand.
- **Reserved padding bytes — per-page** (restart-table entry's
  `Reserved uint8` in `page-formats.md §Compressed Leaf`, the
  uncompressed-leaf header's `Reserved uint16` in `§Uncompressed
  Leaf`). *Ignored on read*: must be zero on write, but readers
  tolerate any value for forward-compatibility — these slots
  exist precisely to be repurposed without a format break, since
  each leaf page is self-describing and a mixed-version
  keyspace can hold pages from multiple encoder eras.
- **Reserved bytes in fixed-format meta structures** (keyspace
  descriptor `Reserved [3]byte` in `keyspaces.md §Keyspace
  Descriptor`; future analogous slots in the meta page). *Strict-
  reject* on read: the descriptor is part of the meta-page atomic
  unit and extending it requires a meta-page format version bump,
  not a silent per-record extension. Forward-compatibility for
  these slots is achieved at the file-format level, not by
  ignoring stray bytes.

When a per-page reserved slot is consumed by a future format,
the spec promotes the corresponding padding byte(s) from
"reserved" to the field's name; readers compiled against the new
spec read it normally; readers compiled against the old spec
continue to ignore it (degraded but safe). When a meta-format
reserved slot is consumed, the meta-page format version bumps
and the strict-reject contract enforces a clean cutover. Spec
sites should cite this section by category rather than restate
the policy.

A page's ID is implicit — computable from its file offset
(`offset / PageSize`). This avoids wasting 8 bytes per page on
redundant information and eliminates any possibility of inconsistency
between a stored PageID and the actual file position. `Check()`
verifies page type and structural validity at each offset; no stored
PageID is needed.

When `PageChecksum` is enabled (the default), every data page
(branch, leaf, overflow, RPL segment) carries an 8-byte xxhash64
footer in the last 8 bytes of the page. See `checksums.md`.

## Meta Page

Two meta pages occupy pages 0 and 1. The writer always updates the
one NOT currently active. Meta pages do not carry the standard page
header.

```
Meta Page
+------------------+
| Magic            | uint32 - identifies file as gmdb
| Version          | uint32 - format version
| PageSize         | uint32 - page size in bytes
| Flags            | uint32 - bit 0: PageChecksum (immutable); bit 1: Checkpoint (mutable); bits 2-31: reserved
| BitmapPages      | uint32 - number of pages in the allocation bitmap
| Padding          | 4 bytes
| UUID             | [16]byte - database identity, generated at creation, immutable
| MinSize          | uint64 - minimum database size in pages
| MaxSize          | uint64 - maximum database size in pages
| GrowStep         | uint64 - growth step in pages
| ShrinkThreshold  | uint64 - shrink threshold in pages
| HighWaterMark    | uint64 - first unallocated page ID
| RPLHeadPage      | uint64 - page ID of the newest RPL segment (0 = empty)
| RPLTailPage      | uint64 - page ID of the oldest RPL segment (0 = empty)
| RPLEntryCount    | uint64 - total entries across all RPL segments
| NumFreePages     | uint64 - total free pages (set bits in bitmap)
| KeyspaceRoot     | uint64 - root page of keyspace B+tree
| NumKeyspaces     | uint64 - number of keyspaces
| TxnID            | uint64 - transaction ID that wrote this meta
| Checksum         | uint64 - xxhash64 of all preceding bytes
+------------------+
```

Total meta-page payload: 4×4 + 4 + 4 + 16 + 13×8 = 144 bytes. Fits
comfortably in any supported page size (min 4 KB).

`UUID` is a 128-bit random identifier generated at database creation
time and copied identically to both meta pages. Used for backup
validation and lock-file association. Immutable.

`Flags` policy: `Open()` must reject databases where any unknown flag
bit is set. Bit 0 (`PageChecksum`) is immutable. Bit 1 (`Checkpoint`)
is mutable — set/cleared per commit depending on whether the
commit's data pages have been confirmed on stable storage (see
`durability.md`).

The file-format fields (`MinSize`, `MaxSize`, `GrowStep`,
`ShrinkThreshold`) persist across opens.

The **active** meta page is the one with the highest `TxnID` whose
checksum is valid (subject to the checkpoint-flag precedence rule of
`durability.md`). A crash mid-write to the meta page leaves an
invalid checksum and the database falls back to the other meta page,
which points to the previous consistent state.

## Byte order

All multi-byte integers are stored in little-endian byte order.

## Initialization

The state of meta pages at creation, including the first valid
`TxnID = 0` and the empty keyspace root, is defined in
`api-surface.md §Database Initialization`.

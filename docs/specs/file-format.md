# File Format Parameters

The database file size is managed dynamically between configurable
lower and upper bounds. The file-format fields are stored in the
meta page (see `file-layout.md`) and persist across opens.

Scope:
- File-format parameters (`MinSize`, `MaxSize`, `GrowStep`,
  `ShrinkThreshold`).
- `MaxSize` immutability.
- File growth and shrinkage protocols.

Depends on / interacts with:
- `file-layout.md` for storage of the format fields in the meta
  page and for the bitmap-region derivation from `MaxSize`.
- `mmap-strategy.md` for the `MaxSize`-sized mmap reservation
  and the VMA inheritance property used by `ftruncate`.
- `free-space.md §Tail Page Refund` for the trigger that drives
  shrinkage.

## Invariants

Invariant: kind=clause-explicit;
  property=`MaxSize` is set at creation, stored on the meta page,
    and immutable for the life of the database. Re-`Open()`ing
    with a different `Options.FileFormat.Upper` does not change
    it; the only way to change `MaxSize` is `CopyTo(path,
    compact)` to a new database;
  from=this spec §`MaxSize` is immutable;
  violation=Changing `MaxSize` would shift the data-page region
    (whose start depends on `BitmapPages`, which depends on
    `MaxSize`) — every existing page ID points to the wrong file
    offset, corrupting every reachable tree page.

Invariant: kind=clause-explicit;
  property=File growth via `ftruncate` always grows to a
    `GrowStep`-aligned size, clamped to `MaxSize`. Exceeding
    `MaxSize` returns `ErrDBFull` from `pageAlloc()`;
  from=this spec §File Growth;
  violation=Un-clamped growth past `MaxSize` allocates pages
    outside the mmap reservation, SIGBUSing on first access;
    unaligned grow defeats the OS readahead behaviour `GrowStep`
    is sized for.

Invariant: kind=clause-explicit;
  property=File shrinkage via `ftruncate` only truncates the
    region beyond `HighWaterMark`. Pages at offsets `<
    HighWaterMark` are never truncated, regardless of free-bit
    state;
  from=this spec §File Shrinkage;
  violation=Truncating below `HighWaterMark` SIGBUSes the next
    reader that touches a still-referenced page beyond the new
    truncation boundary — silent data loss + crash.

Invariant: kind=clause-explicit;
  property=`MinSize`, `GrowStep`, and `ShrinkThreshold` are
    mutable via `Tx.SetFileFormat()`; `MaxSize` (alias
    `FileFormat.Upper`) is rejected at `SetFileFormat()` if
    different from the stored value;
  from=this spec §File Format Parameters;
  violation=A mutable `MaxSize` re-creates the
    bitmap-region-shift corruption vector above; the API guard
    is the only protection.

## File Format Parameters

| Parameter | Meta Field | Description | Default |
|-----------|-----------|-------------|---------|
| Lower bound | `MinSize` | Minimum file size in pages. | `2 + BitmapPages` |
| Upper bound | `MaxSize` | Maximum file size in pages. Determines mmap reservation and bitmap size. **Immutable after creation.** | 256 GB / `PageSize` |
| Growth step | `GrowStep` | Pages to grow by when extending. | 65 536 (256 MB at 4 KB) |
| Shrink threshold | `ShrinkThreshold` | Shrink when `fileSize - HighWaterMark > threshold`. | 131 072 (512 MB at 4 KB) |

Set at creation via `Options` and persisted. `MinSize`, `GrowStep`,
and `ShrinkThreshold` can be modified via `Tx.SetFileFormat()` —
see `api-surface.md`.

### `MaxSize` is immutable

The bitmap region size is fixed at creation (it depends on
`MaxSize` and `PageSize`); changing `MaxSize` would shift all
data-page offsets and invalidate every page ID. To change
`MaxSize`, use `CopyTo(path, compact)` to create a new database.

## File Growth

When `pageAlloc()` needs to extend:

1. `newSize = alignUp(HighWaterMark + needed, GrowStep)`.
2. Clamp to `MaxSize`. Exceeded ⇒ `ErrDBFull`.
3. `ftruncate()` the file. The existing mmap (sized to `MaxSize`)
   covers the new pages automatically — no second `mprotect`
   call is needed, because the `mprotect(PROT_READ)` applied at
   Open covers the full `MaxSize` virtual reservation, and the
   newly file-backed pages inherit `PROT_READ` from that
   reservation. This inheritance holds on Linux, macOS, and
   FreeBSD because `MAP_SHARED` over the reservation is a
   single VMA that `ftruncate` does not split, so the VMA's
   protection applies uniformly to the newly-backed pages
   without additional syscalls. Ports to other OSes must
   re-verify this property.

## File Shrinkage

After the commit point, if file size exceeds `HighWaterMark` by
more than `ShrinkThreshold`:

1. `newSize = alignUp(HighWaterMark, GrowStep)`.
2. Clamp to `MinSize`.
3. Shrink DEFERS while any reader transaction is VISIBLE in the
   reader table at the gate's scan: a reader's file-resident page
   bound is fixed when it begins, and truncating under a live
   reader would turn a corrupt content-derived page id — the input
   class `checksums.md` §Structural and Allocation Bounds promises
   `ErrCorrupted` for — into a SIGBUS on the newly-unbacked region.
   Legitimate trees never reference the truncated range
   (`HighWaterMark` plus the reclamation bound guarantee that), so
   the deferral protects only the hardened corrupt-input path; the
   shrink retries at the next eligible commit. The lock-free
   reader-CAS acquisition window is closed by the SHRINK SEQLOCK
   (`ShrinkSeq` in the lock-file header): the writer increments it
   to ODD immediately before the reader-visibility scan and to
   EVEN after the truncate lands (or after deferring), serialised
   under the write grant; a reader brackets its file-size read —
   slot publish, read the counter, fstat, re-read — and an odd or
   changed counter means a truncate span overlapped, so it
   re-reads the size until the bracket is clean. A writer that
   CRASHES mid-span leaves the counter odd; stale-writer recovery
   re-evens it, and a reader's bracket retry is CAPPED (a dead
   writer's truncate either never ran or already settled, so the
   post-cap size read is stable). **Accepted residual**, narrowed:
   a LIVE writer whose scan→truncate span outlasts the reader's
   full retry budget — by being descheduled mid-span, or by an
   `ftruncate` of a very large mapped region legitimately taking
   that long (page-cache invalidation + PTE teardown across every
   peer's mapping) — while that reader exhausts the cap.
   Corrupt-input-only (never a legitimate read), and
   multiplicatively rarer than the pre-seqlock any-acquisition
   window. Latency note: a counter left ODD by a crashed writer
   taxes every BeginRead with the full retry budget until the next
   writer's grant acquisition re-evens it — a read-only aftermath
   pays it per-begin. (Pinned by
   `TestReaderBracketsShrinkSeqlock`.)
4. `ftruncate()`. The mmap reservation remains at `MaxSize`.

Automatic and zero-overhead — happens as a natural consequence
of tail-page refund during commit (see `free-space.md §Tail
Page Refund`). No explicit compaction needed for the common
case.

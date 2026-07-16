# mmap Strategy

Memory-mapping policy for the data file: how the mapping is sized,
how it remains stable across file grow/shrink, and which `madvise`
hints are applied (prefaulting, huge pages, read-cooldown).

Scope:
- Read-only mapping protections.
- Read-path and write-path access patterns through the mmap.
- Mmap sizing relative to `MaxSize` and how grow/shrink work
  without re-mmap.
- Opt-in `madvise` hints: `MADV_POPULATE_READ`, `MADV_HUGEPAGE`,
  `MADV_COLD`.
- `ReadOnly` mode semantics on the mmap.

Depends on / interacts with:
- `pager-slab.md` for the writer's read-through-mmap +
  pwrite-on-write protocol.
- `file-format.md` for `MaxSize` immutability and grow/shrink
  bounds.
- `file-layout.md` for `HighWaterMark` as the file-tail
  boundary readers must respect.

## Invariants

Invariant: kind=clause-explicit;
  property=The data file is mapped `MAP_SHARED | PROT_READ` in
    every process (including the writer); `mprotect(PROT_READ)`
    is applied after Open as a belt-and-suspenders guard. The
    writer never writes through the mmap pointer;
  from=this spec §Read Path + §Write Path;
  violation=A `PROT_WRITE` mmap admits stray-pointer or `unsafe`
    misuse to silently corrupt the data file — the very failure
    mode this design avoids. It also resurrects the macOS
    `msync(MS_SYNC)` special case in the commit path.

Invariant: kind=clause-explicit;
  property=The mmap reservation is sized to `MaxSize`. The
    mapping does not change as the file grows or shrinks; only
    the file's actual length changes (via `ftruncate`). Readers
    must not access pages beyond `HighWaterMark` (from the
    active meta);
  from=this spec §mmap Resizing;
  violation=Accessing the unmapped portion of the reservation
    raises SIGBUS in the reader goroutine — process-fatal. The
    `HighWaterMark` guard is the only protection.

Invariant: kind=entailed;
  property=A successful pwrite of a page from this process is
    observable through any process's `MAP_SHARED | PROT_READ`
    mapping of the same file without an explicit `msync` call;
  from=entailed: Linux and macOS unified-page-cache property for
    `MAP_SHARED` + pwrite on the same file (`pager-slab.md`);
  violation=A platform that splits the page cache between mmap
    and pwrite breaks cross-process visibility of commits — the
    pager-slab commit protocol's last step (atomic meta swap)
    would not be observable to other processes without an
    additional sync. Ports to such platforms must add an
    `msync` to the commit path.

Invariant: kind=clause-explicit;
  property=Newly file-backed pages added by `ftruncate` (file
    growth) inherit `PROT_READ` from the existing VMA. No
    additional `mprotect` is required on Linux, macOS, or
    FreeBSD because `MAP_SHARED` over the reservation is a
    single VMA that `ftruncate` does not split;
  from=this spec §mmap Resizing + `file-format.md §File Growth`;
  violation=Ports to other OSes that split the VMA on
    `ftruncate` may leave new pages without `PROT_READ` — a
    stray pointer can corrupt the file again. The port must
    re-verify this property and add an `mprotect` if needed.

## Read Path

All processes mmap the data file with:

```
MAP_SHARED | PROT_READ
```

Page lookup is `mmap[pageID * pageSize]` — one level, no branches.
Branch + leaf page reads go directly through this mmap. The OS
page cache serves the data.

`Options.ReadOnly` controls whether the writer path is initialised
at all (the data mmap mode does not change — it is always
read-only). When `ReadOnly = true`, the writer pager is not built,
no background maintenance runs, and the write entry points
(`Begin` / `Update` / `Batch` / `Compact` / `Checkpoint`) return
`ErrDatabaseReadOnly`. The `writer flock-grant goroutine` is not
started — a read-only handle never takes `LOCK_EX`.

A read-only handle still participates in the reader-table protocol
so a concurrent cross-process *writer* cannot reclaim pages out
from under an in-flight read: when the lock file can be opened
read-write, `Open` starts the heartbeat goroutine and each read
transaction acquires a reader slot exactly as a read-write handle
does (cross-process.md §Reader Table). Only on truly read-only
media — where the lock file cannot be opened read-write — does
`Open` fall back to lock-free snapshot reads (logging a warning);
in that fallback a writer on shared storage could reclaim pages
under a reader (torn reads), but a read-only medium normally
precludes any writer. Suitable for read-only media or read-only
filesystem permissions.

## Page Memory Management

OS-managed. Reads through the mmap are file-cache-backed; the
kernel handles eviction under memory pressure. No application-
level page buffer, no eviction algorithm, no page-count limit.
The OS has global visibility into memory pressure across all
processes and is better positioned to make eviction decisions.

The writer additionally holds slab buffers (page-sized) for pages
it has CoW'd in the current transaction. Steady-state slab usage is
bounded by `Options.MaxTxBufferBytes` — the SPILL threshold: excess
pages are written out to their allocated file locations at
operation boundaries rather than capping transaction size (see
`pager-slab.md §Slab Budget`).

## Write Path

The writer does **not** modify the mmap. All modifications:

1. Read current page content via `pager.Page(id)` (which checks
   `dirty[id]` first, falling back to mmap).
2. Allocate fresh page ID + slab buffer; copy old content into
   the buffer.
3. Mutate the buffer.
4. At commit: pwrite buffers, bitmap pages, then meta page — see
   `pager-slab.md §Commit Write Ordering`.

There is no platform-specific code in the commit path. Linux,
macOS, and FreeBSD all use `pwrite + fdatasync`. No
`msync(MS_SYNC)` is needed because the writer never writes
through the mmap.

## mmap Resizing

The mmap region is sized to `MaxSize`. This over-allocates virtual
address space — only the file-backed portion is usable, but the
mapping does not need to change as the file grows or shrinks. The
unmapped region beyond the file size will SIGBUS if accessed, so
readers must check `HighWaterMark` from the meta page.

`MAP_SHARED` file-backed mappings are not charged against Linux
`vm.overcommit_memory` accounting (the file is the backing
store). But per-process `RLIMIT_AS` does apply to virtual-address-
space reservations regardless of mapping type. Most defaults are
unlimited; restricted environments may need a lower `MaxSize`.

## Prefaulting (Linux 5.14+)

When `Options.PreloadPages` is true, the database calls
`madvise(MADV_POPULATE_READ)` on the file-backed portion of the
mmap (pages 0 through `HighWaterMark - 1`) at open time. Pre-
faults all pages into the OS page cache, eliminating page faults
on first access.

- **Predictable latency.** First read txn doesn't pay per-page
  fault costs.
- **Sequential I/O.** Kernel reads pages sequentially during
  prefault — more efficient than random-access demand paging.

`MADV_POPULATE_READ` (Linux 5.14+) works on `MAP_SHARED` and
returns errors synchronously. Silent no-op on older kernels.

Prefaulting is also performed internally during `CopyTo()` on
the source database's mmap.

Default: false — most workloads benefit from demand paging where
only accessed pages enter the cache.

## Huge Pages (Linux)

When `Options.HugePages` is true, the database calls
`madvise(MADV_HUGEPAGE)` on the data file mmap. Enables
transparent huge page (THP) backing, allowing the kernel to use
2 MB pages instead of 4 KB.

- **Reduced TLB pressure.** A 1 GB database drops from 262 144
  TLB entries to 512 (4 KB → 2 MB).
- **Fewer page faults.** Each fault maps 2 MB instead of 4 KB.

THP for file-backed `MAP_SHARED` is mature on Linux 6.x. Kernel
promotes opportunistically based on alignment and availability.

Default: false. Ignored on non-Linux and on kernels without THP
for file-backed mappings.

## Read Transaction Cooldown (Linux 5.4+)

When `Options.ReclaimOnClose` is true, closing a read transaction
calls `madvise(MADV_COLD)` on the mmap region the transaction
accessed. Hints the kernel that the pages are no longer actively
used and may be reclaimed under memory pressure.

Useful for batch processing workloads with large sequential
scans (exports, analytics queries). Without `MADV_COLD`, scanned
pages remain in the cache, potentially evicting more useful
pages.

The implementation tracks min/max page IDs accessed during the
transaction (two atomic min/max updates per page read) and
issues a single `madvise(MADV_COLD, min*PageSize,
(max-min+1)*PageSize)` on close.

Default: false. Silent no-op on non-Linux or kernels < 5.4.

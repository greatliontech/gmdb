# Checksums

gmdb uses a single hash algorithm — **xxhash64**
(`github.com/cespare/xxhash/v2`, `xxhash.Sum64`) — across the
entire file: meta-page checksum (mandatory) and data-page footers
(opt-out, on by default). One hash family means one implementation,
one performance profile, and no algorithm version flags.

Scope:
- Meta-page checksum: always-on, atomic-commit anchor.
- Data-page footer: enabled-at-creation flag, immutable for the
  life of the file.
- Storage layout, algorithm, verification, computation rules.
- What checksums catch and what they do not.

Depends on / interacts with:
- `file-layout.md` for the meta-page `Flags` bit 0 (PageChecksum)
  and meta-page checksum field.
- `pager-slab.md` for the commit-time write protocol that writes
  the footer into each dirty buffer before pwrite.
- `background-maintenance.md` for the checksum scrubbing task
  that proactively verifies footers.

## Invariants

Invariant: kind=clause-explicit;
  property=Every meta page carries an xxhash64 checksum of all
    preceding fields. Recovery accepts only metas whose checksum
    verifies; a meta with an invalid checksum is treated as
    if it does not exist;
  from=this spec §Meta Page Checksum;
  violation=A torn meta-page write surfaces as a structurally
    valid but logically inconsistent meta — readers traverse
    into garbage; the dual-meta fallback exists precisely to
    catch this.

Invariant: kind=clause-explicit;
  property=The `PageChecksum` flag (meta `Flags` bit 0) is set
    at creation and **immutable**. All data pages in a
    checksummed database carry the footer; no data pages in a
    non-checksummed database do;
  from=this spec §Data Page Checksums + `file-layout.md`;
  violation=Mutability would let half a file carry footers and
    half not — `Check()` can't predict which pages should
    verify, and the read path can't tell whether a footer-byte
    region is `[8 bytes of value tail | footer]` or
    `[16 bytes of value tail | nothing]`.

Invariant: kind=clause-explicit;
  property=When checksums are enabled, the footer occupies the
    last 8 bytes of every data page (Branch, Leaf, Overflow,
    RPL segment). The footer covers bytes 0 through
    `PageSize - 9` inclusive — including the page header;
  from=this spec §Storage;
  violation=A footer that omits the header lets a corrupted
    header survive verification — wrong `Type` or `Count` is
    then accepted as authentic.

Invariant: kind=clause-explicit;
  property=Bitmap pages do not carry checksums (no page header
    or footer; the entire page is bitfield data). Bitmap
    integrity is verified indirectly via the meta-page
    checksum and `Check()`'s recomputation of the free-page
    count from the bitmap;
  from=this spec §Storage (bitmap pages);
  violation=Adding a bitmap footer would shorten each bitmap
    page's bit capacity and shift every page-ID-to-bit
    mapping — every existing database would re-derive
    `BitmapPages` and corrupt.

Invariant: kind=clause-explicit;
  property=Per-page verification is cached on the pager: a page
    verified once in a transaction is not re-verified on
    subsequent accesses within the same transaction;
  from=this spec §Verification;
  violation=Re-verifying every access turns checksums from a
    bounded constant per page into a quadratic cost across
    repeated touches — the read-path performance budget
    breaks for keyspaces with high tree-traversal depth.

Invariant: kind=clause-explicit;
  property=Pages CoW'd in the current transaction have their
    footers computed at commit time on the dirty slab buffer,
    before the pwrite. The footer is written into the last 8
    bytes of the buffer;
  from=this spec §Computation (Write Path);
  violation=Computing the footer in mid-mutation reads pre-
    mutation content (footer doesn't match final bytes) or
    forces re-verification on the next access; computing it
    after pwrite would mean writing footers separately,
    breaking the single-pwrite-per-page commit step.

## Meta Page Checksum (Always On)

Both meta pages carry an xxhash64 checksum of all preceding
fields. Mandatory, cannot be disabled. The meta page is the
atomic commit point — a torn write here would silently point
to an inconsistent tree. The checksum detects this and triggers
fallback to the other meta page.

Stored as the trailing `uint64` of the meta page payload (see
`file-layout.md`).

## Data Page Checksums (On by Default)

Data pages (branch, leaf, overflow, RPL segment) carry an 8-byte
xxhash64 footer in the last 8 bytes of the page when checksums
are enabled.

Enabled via `Options.PageChecksum = true` at creation. Default:
true. The setting is stored as a flag in the meta page's `Flags`
field (bit 0) and is **immutable after creation** — all pages in
a checksummed database have checksums; all pages in a
non-checksummed database do not.

The default is on. xxhash64 is fast enough in software (no
hardware-acceleration requirement unlike CRC32C) that the cost is
negligible compared to mmap page-fault and B+tree traversal
costs, and the protection against silent bitrot on commodity
filesystems (ext4 without `data=journal`, xfs without checksums)
is worth the 0.2% page-space overhead.

## Storage

```
Page (with checksum enabled)
+-----------------------+
| Page Header (8 bytes) |
+-----------------------+
| Page Content          |
| (PageSize - 16 bytes) |
+-----------------------+
| xxhash64 (8 bytes)    |  footer: hash of bytes 0 through PageSize-9
+-----------------------+
```

The footer keeps the page header at 8 bytes. Usable content
shrinks by 8 bytes when checksums are enabled — 0.2% at 4 KB.
The checksum covers the entire page from byte 0 through
`PageSize - 9` inclusive, including the page header.

Bitmap pages do not carry checksums (no page header or footer;
the entire page is bitfield data). Bitmap integrity is
guaranteed by the CoW model and the meta page checksum (the
meta references the bitmap indirectly through `NumFreePages`
and the page-allocation invariants that `Check()` verifies).

## Algorithm: xxhash64

`xxhash.Sum64` from `github.com/cespare/xxhash/v2`. Pure Go,
SIMD-accelerated on amd64 / arm64 where the compiler can
vectorise.

- ~4 ns per 64 bytes; ~50–80 ns per 4 KB page in practice.
- Faster than CRC32C in pure software; competitive with
  CRC32C+SSE4.2 on amd64.
- 8-byte output — slightly larger than CRC32C's 4 bytes but a
  stronger hash and consistent with the meta-page checksum.

The same library and algorithm power the meta-page checksum, so
the runtime cost is amortised across one hash implementation in
the binary.

## Verification (Read Path)

When checksums are enabled, every page read from the pager is
verified on first access in a transaction:

1. Compute xxhash64 of bytes 0 through `PageSize - 9`.
2. Compare with the 8-byte footer.
3. Mismatch ⇒ return `ErrBadPageChecksum` with the page ID.

Per-page verification is cached on the pager — a page verified
once in a transaction is not re-verified on subsequent accesses
within the same transaction. For a depth-4 lookup the cost is
~200–320 ns — negligible compared to traversal and potential
page-fault costs. For full-database scans the cost is bounded
by memory bandwidth.

Pages CoW'd in the current transaction have their footers
computed at commit time on the dirty slab buffer, before the
pwrite.

## Computation (Write Path)

When checksums are enabled, the xxhash64 footer is computed on
each dirty slab buffer at commit time, before the pwrite. The
footer is written into the last 8 bytes of the buffer.

## What Checksums Do and Do Not Catch

**Catches:**

- Silent bitrot on disk (bit flips in stored data).
- Firmware bugs in SSD/NVMe controllers that corrupt data at
  rest.
- RAID controller or storage stack corruption.
- Kernel bugs that corrupt the page cache after a successful
  write.

**Does not catch:**

- Torn writes (handled by CoW + meta-page checksum).
- In-memory corruption between buffer-fill and pwrite (the
  checksum is computed on the same buffer that is written —
  if the buffer is corrupt, the checksum matches the corrupt
  data).
- Corruption introduced by the application via stray pointers
  — the data mmap is `PROT_READ` only, so stray writes there
  SIGSEGV immediately; the slab buffer is application memory,
  where typical unsafe-pointer bugs would land. Defense via
  `mprotect` mitigates the most common variant.

## Default

Checksums are **enabled by default** (`Options.PageChecksum =
true`). Disable via `PageChecksum = false` at creation only when
running on a filesystem with end-to-end checksums (ZFS, btrfs,
ReFS) or storage controllers with built-in integrity — and the
0.2% page-space saving is meaningful for the workload.

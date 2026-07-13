# Checksums

gmdb uses a single hash algorithm — **XXH3-64**
(`github.com/zeebo/xxh3`, `xxh3.Hash`) — across every persisted
digest in the file: meta-page checksum (mandatory), data-page
footers (opt-out, on by default), and the index schema fingerprint
(`indexing.md §Drift Guard`). One hash family means one
implementation, one performance profile, and no algorithm version
flags.

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
  property=Every meta page carries an XXH3-64 checksum of all
    preceding fields. Recovery accepts only metas whose checksum
    verifies; a meta with an invalid checksum is treated as
    if it does not exist;
  from=this spec §Meta Page Checksum;
  violation=A torn meta-page write surfaces as a structurally
    valid but logically inconsistent meta — readers traverse
    into garbage; the dual-meta fallback exists precisely to
    catch this.

Invariant: kind=clause-explicit;
  property=Data-page checksums are ON by default: a zero-value
    `Options` (no `DisablePageChecksum` set) creates a database
    whose meta carries `MetaFlagPageChecksum`, so every data page
    is footer-protected. Only `Options.DisablePageChecksum = true`
    clears the flag;
  from=this spec §Default;
  violation=`Open(ctx, path, Options{})` persists the flag clear,
    so data pages carry no footer and the spec-promised bitrot
    detection is silently absent — exactly the drift a plain
    `PageChecksum bool` (zero value = off) invited. Enforced by
    `TestPageChecksumDefaultOnMetaFlag` /
    `TestPageChecksumDefaultOnDetectsBitrot`.

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

Both meta pages carry an XXH3-64 checksum of all preceding
fields. Mandatory, cannot be disabled. The meta page is the
atomic commit point — a torn write here would silently point
to an inconsistent tree. The checksum detects this and triggers
fallback to the other meta page.

Stored as the trailing `uint64` of the meta page payload (see
`file-layout.md`).

## Data Page Checksums (On by Default)

Data pages (branch, leaf, overflow, RPL segment) carry an 8-byte
XXH3-64 footer in the last 8 bytes of the page when checksums
are enabled.

Checksums are ON by default: a zero-value `Options` creates a
checksummed database. Opt out with `Options.DisablePageChecksum =
true` at creation (the flag turns the footer OFF; its zero value
leaves checksums enabled). The setting is stored as a flag in the
meta page's `Flags` field (bit 0) and is **immutable after
creation** — all pages in a checksummed database have checksums; all
pages in a non-checksummed database do not.

The default is on. XXH3-64 is fast enough in software (no
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
| XXH3-64 (8 bytes)    |  footer: hash of bytes 0 through PageSize-9
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

## Algorithm: XXH3-64

`xxh3.Hash` from `github.com/zeebo/xxh3`. Pure Go with
SIMD-accelerated paths (AVX2/SSE2 on amd64) and a portable
generic fallback.

- ~97 ns per 4 KB page, ~1.4 µs per 64 KB page (measured; ~42
  GB/s) — far past CRC32C or XXH64 in pure software.
- 8-byte output — slightly larger than CRC32C's 4 bytes but a
  stronger hash and consistent with the meta-page checksum.
- XXH3-64 is the ONLY digest any engine version verifies — there
  is no dual-verification or alternate-algorithm path; a file
  whose digests were computed by another algorithm fails
  verification and is rejected at the format-version gate first.

The same library and algorithm power the meta-page checksum and
the index schema fingerprint (`indexing.md §Drift Guard`), so one
hash family covers every persisted digest in the file.

## Verification (Read Path)

When checksums are enabled, every page read from the pager is
verified on first access in a transaction:

1. Compute XXH3-64 of bytes 0 through `PageSize - 9`.
2. Compare with the 8-byte footer.
3. Mismatch ⇒ return `ErrBadPageChecksum` with the page ID.

Per-page verification is cached on the pager — a page verified
once in a transaction is not re-verified on subsequent accesses
within the same transaction. For a depth-4 lookup the cost is
~200–320 ns — negligible compared to traversal and potential
page-fault costs. For full-database scans the cost is bounded
by memory bandwidth.

RPL segment pages are read outside the verifying page accessor
(the reclamation walk, the Open-time chain rebuild, and Check's
chain walk all use the raw accessor), so each of those walkers
verifies every segment's footer itself before decoding — when the page checksum is enabled;
with checksums off they fall back to structural decode plus the
bounds below, and an in-range wrong entry in a decodable segment
is then inherently undetectable (the trade the
`DisablePageChecksum` option states). A checksum-bad segment is quarantined by
reclamation (bounded leak, reclamation continues past it); the
Open walk rejects a checksum-bad head (the malformed-head
convention) and treats a checksum-bad non-head as the stale-tail
boundary. Reclamation additionally bounds the segment page id and
every decoded entry against the bitmap's allocatable range before
freeing anything, whole-segment-atomically — a decodable segment
with a forged entry must quarantine, never free a live in-range
page unchecked or crash on an out-of-range one (integrity.md's
error-not-crash contract). (Enforced in `reclaimRPL` /
`rebuildRPLChain`; pinned by the checksum-mismatch and
out-of-range-entry quarantine tests in
`internal/pager/freespace_test.go` and the corrupt-chain reopen
tests in `rpl_recovery_test.go`.)

Pages CoW'd in the current transaction have their footers
computed at commit time on the dirty slab buffer, before the
pwrite.

## Structural and Allocation Bounds (Read Path)

Checksum verification catches accidental bitrot, but it does not
cover a deliberately-forged page whose footer was recomputed, nor
a checksum-disabled database. The read path is therefore hardened
independently of the checksum so it never *crashes* on corrupt
input — it returns an error instead:

- **Page-id reachability bound.** Every content-derived page id
  (a branch child pointer, an overflow-run page, a nested-subtree
  root) is bounded against the file-resident page extent
  (`min(fileSize / PageSize, MaxSize)` — the file can be externally
  grown past the mmap reservation) before any mmap access. An out-of-range
  id yields `ErrCorrupted`, never a SIGBUS on the unbacked region
  of the `MaxSize` mmap reservation (the reservation spans
  `MaxSize` but only the first `fileSize` bytes are file-backed;
  see `mmap-strategy.md`). The bound lives in the pager's
  verifying page accessor (`pager-slab.md`). Whole-tree walkers
  that read through the RAW (unverified, unbounded) accessor —
  `Check`'s structural walk and `CopyTo`'s verbatim enumeration —
  carry the same bound themselves: each clamps its HighWaterMark
  walk bound to this extent before walking, so a meta whose
  HighWaterMark outruns the file (a truncated transfer, a forged
  meta) yields `ErrCorrupted` from the walk, never a SIGBUS.
  (Pinned by `TestCopyToTruncatedSourceErrors`.)
- **Overflow-run header cross-check.** The shared tree walk
  validates each overflow run's first-page header — the
  `TypeOverflow` tag and an `AdditionalPages` count consistent
  with the leaf reference's `TotalLen`-derived run — because the
  read path rejects exactly that mismatch at assembly time. A walk
  without the cross-check reports a database clean while every
  `Get` of the affected key fails `ErrCorrupted` (reachable with
  checksums disabled or a recomputed footer). (Pinned by
  `TestCheckReportsCorruptOverflowHeader`.)
- **Allocation bound.** An allocation sized from an on-disk
  length/count field — an overflow value's `TotalLen`, an index
  registry entry's column/covering counts, the meta-page
  `BitmapPages` driving Open's bitmap rebuild — is bounded before
  it is made, so a forged field cannot drive an out-of-memory
  abort. For overflow this means the run length is computed
  without uint32 truncation and the run is read one page at a time
  so it aborts at the file-resident bound before the value buffer
  is allocated. For Open's bitmap rebuild this means BitmapPages
  is bounded against `min(fileSize/PageSize, MaxSize)` (the
  file-resident extent, same pattern as the Page-id reachability
  bound above) AND against the MaxSize-derived capacity
  (`BitmapPages * PageSize * 8` bits must be ≥ MaxSize pages) so
  `bitmap.New` cannot panic with totalPages-exceeds-capacity. An
  unrecoverable runtime OOM throw from `make` is the strongest
  failure shape in this class — `recover()` does not catch
  `runtime.throw`, so the bound MUST precede the `make`.
- **Branch structure validation.** A branch page is validated
  (`ValidateBranch`) before its cell directory is iterated — the
  analogue of the per-leaf `LeafReader.Validate`, applied at the
  first resolver of every branch on a descent. A forged directory
  yields `ErrCorrupted`, not an out-of-bounds panic.

These bounds are enforced by the btree / page / db corruption
tests, not by a spec clause; they are the structural companions
to the checksum verification above, and together they make
`Check()` and ordinary reads tolerant of an arbitrarily-corrupt
on-disk surface.

## Computation (Write Path)

When checksums are enabled, the XXH3-64 footer is computed on
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

Checksums are **enabled by default** — a zero-value `Options`
creates a checksummed database. Opt out with
`Options.DisablePageChecksum = true` at creation only when running
on a filesystem with end-to-end checksums (ZFS, btrfs, ReFS) or
storage controllers with built-in integrity — and the 0.2%
page-space saving is meaningful for the workload. The flag is
inverted (a `Disable…` bool) precisely so the zero value delivers
the protective default; a plain `PageChecksum bool` would have made
the unprotective state the default-constructed one.

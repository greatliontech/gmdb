# Indexed `BulkLoad` external sort uses a single-pass k-way merge with unbounded run fan-in

**Lands:** profiling-driven — when a real indexed `BulkLoad` workload
spills enough sort runs that the single merge pass approaches the
process file-descriptor limit (or its aggregate read-buffer memory
becomes material), OR opportunistically when the external-sort code is
next touched.

## Problem

`indexSorter` (chunk-8.6, `bulkload_indexed.go`) spills a sorted run to
`ScratchDir` each time its in-memory chunk exceeds
`budget = MaxTxBufferBytes / #indexes`. At build time, `newMerger` opens
**every** spilled run simultaneously for a single k-way merge pass: one
open `*os.File` + a 64 KiB `bufio.Reader` + one min-heap slot per run.

So the merge's resource use is `O(#runs) = O(inputBytes / budget)`, not
`O(depth)`. For a pathologically small `MaxTxBufferBytes` against a very
large input (e.g. a multi-gigabyte gitfs SQLite → gmdb migration with a
deliberately tiny buffer), `#runs` can reach thousands — enough to hit
the per-process FD limit (`ENFILE`/`EMFILE`) or to make the aggregate
64 KiB-per-run read buffers material.

## Severity / why deferred

Not a correctness defect. With the default `MaxTxBufferBytes` (256 MiB)
`#runs` is tiny for any realistic input, so this is unreachable in
normal operation. If it *were* hit, `newMerger`'s `os.Open` returns the
FD-exhaustion error, which propagates as a clean `BulkLoad` abort
(bounded leakage, pre-state preserved) — a graceful failure, not
corruption. `bulkload.md §Interaction with Indexes` specifies the
"final merge pass" as single-pass and imposes no cascaded-merge
requirement, so the current shape is spec-conformant.

## Proposed remediation

Cascaded (multi-pass) merge: cap the simultaneous fan-in at a fixed
`maxMergeFanIn` (e.g. 64–128). When `#runs` exceeds the cap, merge runs
in groups into fewer, larger intermediate runs and repeat until the
final group fits the cap. Bounds open FDs + read buffers at
`O(maxMergeFanIn)` regardless of input size, at the cost of extra
scratch I/O passes (`O(log_fanin(#runs))`).

## Acceptance

When this lands, a `BulkLoad` of an input producing `#runs >
maxMergeFanIn` (forced via a tiny `MaxTxBufferBytes`) completes
correctly while never holding more than `maxMergeFanIn` run files open
at once — verifiable by asserting peak open-file count or by a unit
test on the cascaded-merge driver. Existing chunk-8.6 spill tests
(`TestKeyspaceBulkLoadIndexedSpills`, `...UniqueViolationSpilled`) must
still pass byte-identically.

## Notes

Surfaced by the chunk-8.6 Round-1 adversarial review (L-2). Adjacent /
scaling-only — the merge code is new in chunk 8.6 but the failure mode
requires a pathological `MaxTxBufferBytes`-vs-input ratio that the
default configuration never reaches.

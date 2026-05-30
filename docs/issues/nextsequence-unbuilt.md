# Keyspace.NextSequence / SetKeyspace.NextSequence unbuilt (descriptor field serialized but never used)

**Lands:** proactive — spec'd public method; the persistence field is
already wired but never incremented or read.

**Severity:** [M]

**Source:** 2026-05-30 completeness pass (this audit session).

**Governing spec:** `docs/specs/api-surface.md:1014`.

## Problem

`Keyspace.NextSequence()` / `SetKeyspace.NextSequence()` (a monotonic
per-keyspace counter, à la bbolt's `Bucket.NextSequence`) are spec'd but
unimplemented. Tellingly, the persistence is **half-wired**: the `NextSeq`
descriptor field is already serialized
(`internal/page/keyspace_descriptor.go:40-42, 105`) but is **never
incremented or read** by any code path. This is a stub for an unbuilt
feature, not dead state — exactly the pattern this audit session was
chartered to flesh out rather than remove.

## Fix

Implement `NextSequence()` on both keyspace kinds: read the persisted
`NextSeq` from the descriptor, increment, persist it through the normal
descriptor-update path (CoW, dirty-marked, committed atomically with the
tx), and return the new value. Add a regression test asserting
monotonicity across commits and that the value survives reopen.

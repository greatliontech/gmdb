# Incremental compaction relocations re-land in the evacuation band: no progress

Lands: 21

## Finding

**[M] Relocated pages draw from the LIFO allocation hint, which the
pass's eager reclaim just pointed into the evacuation band — the spec's
convergence argument assumes a low-hole allocator that doesn't exist.**
`incremental_compaction.go:336-346` (`ReclaimFreeSpace` then
`compactForest`), `internal/pager/freespace.go:531-535` (`reclaimRPL`
ends with `SetHint(lastReclaimed)`), `internal/btree/relocate.go:214,277`
(plain `pw.AllocPage()`), `freespace.go:171-173` (`FindFirst` scans
forward from the hint). background-maintenance.md §Incremental
Compaction step 2 requires the fresh id to come from "a low free hole"
via "the consolidating allocator" and derives convergence from it; only
the RPL chain-prefix copies enforce below-floor allocation
(`FindFirstBelowFrom`). Steady state: pass N's eager reclaim frees pass
N−2's band pages (high ids), the hint lands there, pass N's relocations
refill the band; relocated pages keep ids ≥ floor; HWM cannot
tail-refund past the refilled band; the fragmentation trigger keeps
firing → unbounded repeated write amplification with no shrink.
Effectiveness/liveness defect and a direct code↔spec divergence — not
corruption.

## Fix direction

Give tree-page relocation a below-floor allocation mode (the mechanism
`FindFirstBelowFrom` already provides for RPL copies), per the spec's
consolidating-allocator clause; spec wins by default. Regression:
multi-pass compaction on a fragmented fixture asserts monotone floor
progress / eventual trigger quiescence, not just net shrink
(`TestCompactionShrinksFileMonotonically` passes on a plateau).

## Provenance

2026-07-10 defect audit; free-space reviewer.

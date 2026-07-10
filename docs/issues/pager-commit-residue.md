# Pager commit/checkpoint residue: under-anchor, armed relocation floor, probe undercount

Lands: 9

## Findings

**[L] Checkpoint under-anchors versus spec.** `checkpoint.go:126-158`:
after step 2's completed fdatasync the code neither advances the
anchored epoch to the pre-bump assertion nor persists it; step 3 writes
a possibly-older `AnchoredTxnID`. durability.md §Checkpoint mechanics
step 3 says to persist the pre-bump anchored value; the commit path
does the analogous advance (`commit.go:142`). Conservative direction
only (peers derive a lower reclamation bound → delayed reclamation).

**[L] An armed RPL relocation request survives a failed compaction
commit and executes inside the next unrelated user commit.**
`incremental_compaction.go:362` arms `rplRelocFloor`; consumed only in
`commitStep0` (`internal/pager/rplreloc.go:65`). A compaction tx failing
before `pgr.Commit` (reachable via `flushKeyspaces` → ErrTxTooLarge,
`tx.go:346-353`) rolls back without clearing it (`AbortTx`/`BeginTx`
don't, `pager.go:538-580, 595-630`); the next user commit performs
unrequested chain-prefix relocation work. Mechanically safe; violates
one-shot ownership. Fix: clear in `AbortTx`/`BeginTx`.

**[L] Chain-prefix relocation probe under-counts the commit budget.**
`internal/pager/rplreloc.go:86` probes only the k copy buffers; the k
retirements appended at `:164` can cross an RPL segment boundary
needing extra segment pages never projected (FreePage's admission check
is skipped `inCommit`, `freespace.go:401`) → ErrTxTooLarge *after*
relocation state changes. Rolled back cleanly and retried at half
budget (never user-visible), but free-space.md §RPL segment relocation
requires probe-then-decline ("no state change until … the prefix fits
the work budget"). Fix: include projected extra segment pages in the
probe.

## Provenance

2026-07-10 defect audit; pager/commit and free-space reviewers.

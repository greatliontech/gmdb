# LaggingReaderAbort can veto probed extension headroom mid-relocation

Lands: when free-space.md enumerates the relocation probe obligations
and classifies a LaggingReaderAbort during the segment append as
decline-scope or failure-scope

## Finding

**[L] The relocation probe's extension-headroom term can be vetoed by
AllocPage's lagging-reader tier before the extension tier is
reached.** `internal/pager/rplreloc.go` (probe 2b counts
`maxSizePages − highWaterMark` as available), `freespace.go` tier
order (lagging-reader callback runs BEFORE file extension;
`LaggingReaderAbort` returns ErrDBFull without trying extension).
Interleaving: the app configures `Options.LaggingReader` returning
Abort; a live reader lags the reclamation bound; bitmap free = exactly
the k homes; probe 2b passes solely on extension headroom; appendRPL's
AllocPage — bitmap tier empty (homes claimed), reclamation blocked by
the reader, lagging-reader tier Aborts → ErrDBFull after relocation
state changed (probe-then-decline violated). Recovery is clean
(AbortTx; the next pass re-arms). Reproduces on base — pre-fix this
state failed even without the callback; the availability probe
narrowed the gap to this callback-veto case.

## Fix direction

Depends on the spec call this issue's `Lands:` names: if Abort
mid-append is decline-scope, the probe must model the veto (e.g.
count extension headroom only when no lagging reader blocks the
bound, or consult the callback read-only — note a user callback
invocation is observable, so "read-only" needs definition); if
failure-scope, the ErrDBFull commit error is the contract and this
closes with the spec clause. Minimum reproducer: the
no-page-for-segment-append regression's fixture + a registered
reader pinning the bound + an Abort callback + MaxSize = HWM + 1.

## Provenance

pager-commit-residue remediation, change-set review round 2
(adjacent). Sibling of the folded segment-append availability probe.

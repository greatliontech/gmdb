# Reclamation behind a reclaimed boundary double-frees after a peer's torn, never-published reclamation

Lands: when grant-handoff tear detection (re-arm on stale-writer
grant acquisition) or reclaimed-boundary reclamation gating is
settled

## Finding

**[H] The reclaimed-boundary walker-agreement argument fails for a
surviving handle whose in-memory RPL chain predates a peer's TORN,
never-published reclamation — reclamation behind that boundary
re-creates the double-free the footer/decode gate closes.**

Shape (cross-process, in-spec, arbitrary-subset crash model): P2
commits txn N+1 (chain …X(newer)…Y(older)… pending). P1 Begins
(Resync rebuilds at N+1) and commits txn N+2 whose reclamation freed
Y then X; P1 dies during commit step 1 after the bitmap page holding
X's own bit was pwritten but before the page holding Y's entry bits
and before the meta publish. Disk: meta N+1 unchanged, X's segment
bit free, Y's entries still allocated. P2's next Begin → Resync sees
TxnID N+1 unchanged → no chain rebuild — its in-memory chain still
lists X and Y. P2's maintenance detection walk truncates at X with
RPLWalkReclaimedBoundary (not gated); Y's entries classify leaked
(!reach && !free && !pending); the snapshot-currency guard passes (no
commit ever published). P2 frees Y's entries while Y is still in its
own in-memory chain; its later reclamation frees them again after
possible re-allocation — double allocation, silent corruption.
Exclusive `CheckWithOptions(Repair)` run by P2 reaches the same state.

Second sub-shape (three handles): P2 opens FRESH after P1's death.
Reachability precondition — the LIVE-JOIN path: some process must
hold an active read tx at P2's Open (CountActiveReaders() != 0), so
Open takes AttachLatest, publishes NO recovery commit, and TxnID
never advances. (Without that reader, Open's recovery path publishes
a recovery commit at TxnID+1, every surviving peer's next Begin
Resync-rebuilds its chain from the post-tear image, and the shape
self-heals.) AttachLatest still truncates P2's chain at X
identically, so a same-process chain-containment check would pass —
but a THIRD surviving process P3 (chain predating the tear, TxnID
never advanced) still lists Y; P2's maintenance frees Y's entries as
leaked, P3's later reclamation double-frees. Cross-process in-memory
chains cannot be inspected, so no same-process containment check
closes this.

## Candidate fixes (decision needed)

- **Grant-handoff tear detection**: run the attach-time torn-
  reclamation re-arm (or a chain re-walk) when the write grant is
  acquired after a stale-writer recovery — the root fix; both
  sub-shapes close because every chain then derives from the
  post-tear image. New cross-process mechanism.
- **Gate reclaimed boundaries too** (latch `rplBoundary` on
  RPLWalkReclaimedBoundary): closes both sub-shapes conservatively
  but defeats the post-crash prompt first-pass reclamation
  (background-maintenance.md §Trigger) until the next commit
  advances the chain.

## Provenance

Chunk-20 adversarial review (reclaimed-boundary walker-agreement
audit). Reproduces on base — adjacent to, not introduced by, the
boundary-gating change set. The spec's §Bitmap Leak Reclamation
invariant records the gap as RESIDUAL, not closed.

# Checkpoint leaves the persisted anchor trailing on self-durable metas

Lands: when durability.md §Anchoring records whether an anchored
assertion's sole durable carrier may be rewritten in place (spec
decision on the carrier-loss gap), or a tear-safe persist channel is
specified

## Finding

**[L] In pure SyncDataOnly use, Checkpoint anchors the active meta's
own assertion in process (step 2's completed fdatasync) but does not
persist the advance: a peer adopting the meta derives a reclamation
bound one epoch older than the fsync guarantees.** Delayed
reclamation only — conservative direction, never unsafety.
durability.md §Checkpoint mechanics step 3 as written pwrites the
active slot unconditionally; `checkpointUnderGrant` deliberately
skips it when the meta is self-durable.

## Why the persist is withheld

A self-durable meta is the SOLE durable carrier of its own assertion
(the other slot's sub-record predates it). Persisting the anchor
advance rewrites that carrier in place; the chunk's change-set review
demonstrated the resulting path: step-4 fdatasync fails having
partially flushed the slot (the kernel consumes the writeback error
and marks the page clean, so the torn bytes stay on disk while the
full write stays in the page cache); the handle poisons and the grant
passes; a peer resyncs from the page cache, adopts the advanced
anchored value, reclaims and reuses pages of older epochs; the system
crashes; recovery finds the slot torn, falls back to the other, OLDER
slot — whose tree references the reused pages. Reclamation-bound
over-run, silent corruption. A non-self-durable bump has no such
hazard: the carried-forward sub-record puts the same assertion in
BOTH slots, so a torn rewrite of one never destroys it.

## Fix direction

Spec decision (surfaced with the chunk): either durability.md
§Anchoring accepts the trailing persisted anchor on self-durable
metas as the contract — this issue then closes with a spec clause —
or it specifies a tear-safe persist channel (e.g. peers trust an
advanced anchor only through their own rewrite-plus-fsync, mirroring
the guard recovery's gated Open already requires), and the persist
lands with that mechanism. The in-process advance and the sole-
carrier skip are pinned by `TestCheckpointSelfDurableAnchorsInProcessOnly`.

## Provenance

pager-commit-residue remediation: the first-round implementation
persisted the advance by rewriting the self-durable slot; the
change-set review demonstrated the carrier-loss interleaving and the
persist was withdrawn to this deferral.

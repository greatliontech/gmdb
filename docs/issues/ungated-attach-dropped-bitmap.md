# Non-gated attach paths still trust dropped bitmap writeback

Lands: when the bitmap redirty (durability.md §Anchoring) is
extended beyond the gated recovery arm.

Review-found (adjacent, reproduces on base of 6ad9aa9): the bitmap
redirty runs only under RecoverToDurable, gated on
!PrevLastWriterLive() (db.go:425). A same-process re-Open after
Close classifies live (coord.go:886) and takes AttachLatest — no
redirty — so the DOCUMENTED DurabilityUnknown recovery (re-Open +
Checkpoint) by a still-alive process re-anchors over clean-stale
dropped bitmap bytes: the exact Checkpoint-invariant breach fixed
at the gate. Same exposure: forced Resync after poisoned
publication (init.go:551), peer live join. Fix shape: redirty on
every writable attach that follows a possible failed-sync history,
or narrow to attaches within a poisoned/unclean lineage; spec
amendment rides the fix.

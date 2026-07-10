# Shrink-defer gate: reader-acquisition TOCTOU residual

Lands: 12

## Finding

**[M] The reader-live shrink deferral has an acquisition-window
residual: reader-slot acquisition is a lock-free CAS with no
happens-before edge to the gate's reader-table scan.**
`internal/pager/commit.go` (maybeShrink gate) + `read_tx.go` (slot
CAS then fstat). Interleaving: the writer's shrink-eligible commit
passes the threshold checks → the gate's scan reads the slot as free
→ a reader CASes the slot and fstats size X → the writer ftruncates
to Z < X. The reader retains bound X for its lifetime; a corrupt
content-derived page id in [Z, X) SIGBUSes instead of returning the
contracted ErrCorrupted (checksums.md §Structural and Allocation
Bounds). Corrupt-input-only (legitimate trees never reference the
truncated range), bounded by the writer's scan-to-truncate span, and
recorded as an accepted residual in file-format.md §File Shrinkage.
Note the recovery-commit gate does NOT share this residual: a reader
CASing after that gate's scan restabilizes by re-reading the meta
post-publication and adopts the recovery commit — exactly the
happens-before edge the shrink gate lacks.

## Fix direction

A shared happens-before mechanism, natural to land with the lock-file
work: a shrink sequence counter in the lock-file header (writer bumps
before/after truncate; readers re-fstat when it moved across their
slot-publish + fstat), or the reader bound redesigned to be
truncation-stable. Filed from the file-resident-bounds chunk's
change-set review with the gate landed as the visible-reader closure.

## Provenance

Change-set review of the file-resident-bounds chunk. Adjacent by the
ship gate's diff test: the base tree has no reader gate at all, so
the fault reproduces on base over the FULL reader lifetime; the gate
landed in that chunk narrows it to the acquisition window recorded
here.

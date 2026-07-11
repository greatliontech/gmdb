# leak-detection.md still describes the closed flag as a bare *atomic.Bool

Lands: when leak-detection.md §Cleanup Behavior or §Close
Ordering is next amended for any reason

## Finding

**[L]** Three descriptive clauses predate the gate's promotion to
the composite (closed + txInflight) structure and still call it a
standalone heap `*atomic.Bool`: leak-detection.md:41 ("`db.closed`
is a `*atomic.Bool` allocated on the heap"), :197 (same), :281
("shared-`*atomic.Bool` gate pattern"). The spec's own §Close
Ordering already names `closeGate.CompareAndSwapClosed` /
`closeGate.BeginClose` and the txInflight drain, so the document
is internally inconsistent about the flag's shape. Code is
behaviorally right (the composite gate, now
`internal/closegate.Gate`); the amendment is descriptive
alignment only — same class as the resolved
spec-descriptive-drift batch.

## Provenance

Adjacent finding from the closegate-move change-set review;
pre-existing at that change set's base.

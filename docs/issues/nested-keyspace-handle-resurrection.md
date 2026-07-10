# Child-tx delete+recreate resurrects the parent's dead keyspace handle

Lands: 16

## Finding

**[M] The nested-commit handle merge overwrites a same-name parent
handle in place, resurrecting a handle that DeleteKeyspace's contract
says is permanently dead.** `nested.go:257-271`
(`mergeKeyspaceHandles`; mirrored at `mergeSetKeyspaceHandles:313-327`).
When the child ran `DeleteKeyspace("a")` then `CreateKeyspace("a")`,
the child's open set holds a fresh live handle under the same
`unique.Handle`, so the merge's first loop updates the parent's OLD
handle live instead of the second loop killing it. Reproduced: parent
handle P on "a" → child deletes + recreates "a" → child commits →
`P.Put` returns nil and writes into the recreated keyspace —
api-surface.md's DeleteKeyspace clause and keyspace.go:53-59's godoc
require ErrKeyspaceClosed ("Re-creating the same name … does NOT
reactivate the old handle"). Within-tx delete+recreate already behaves
correctly; only the nested-commit path diverges. The SetKeyspace
variant additionally lets the resurrected handle's FixedValueSize
change under the caller. No corruption; a caller keying recovery logic
on ErrKeyspaceClosed silently writes into a different keyspace
generation.

## Fix direction

Track delete+recreate across the child boundary (e.g. generation
counter on the descriptor or a child-deleted-names set consulted by the
merge): the parent's pre-existing handle is killed, the child's fresh
handle survives as the live one. Regression tests: nested
delete+recreate for both keyspace kinds, asserting the old handle
returns ErrKeyspaceClosed and a freshly opened handle works.

## Provenance

2026-07-10 defect audit; keyspace reviewer, reproducer-confirmed.
`TestDeleteKeyspaceRecreateLeavesOldHandleDead` covers within-tx only.

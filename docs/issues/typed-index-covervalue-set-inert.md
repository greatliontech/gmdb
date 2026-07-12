# typed.Index CoverValue on a SetKeyspace is documented-inert yet pays write amplification

Lands: when typed-keyspaces.md §Covering is next amended

## Finding

**[L]** `typed.Index{CoverValue: true}` on a typed SetKeyspace is
accepted and documented as having "effect only on a
`typed.Keyspace`-backed index" (typed-keyspaces.md §Covering) —
but the write path stores the covering payload per set member
unconditionally, the byte layer never serves covering for set
indexes, and the set handle never enables cover-value return: the
declaration pays fingerprinted write amplification for bytes no
read path can reach. The sibling decl form now REJECTS the same
combination (`typed.ColumnIndex` — typed-columns.md §Covering
projections calls it "a trap, not an option"), so the two typed
forms disagree. Amending `typed.Index` to reject is a
documented-contract change — spec-amend channel, user decides;
the rejection machinery (the SetKeyspace factories' probe) is
already in place to extend.

## Provenance

Adjacent finding from the covering-projections change-set review;
the inconsistency became visible when the column tier's rejection
landed.

# Descriptor validation errors carry a misleading "page:" prefix

Lands: when internal/descriptor is next amended

## Finding

**[nit]** `internal/descriptor.Validate`'s error strings are
prefixed `"page: keyspace descriptor …"` — the codec never lived
in `internal/page`, and the package is now `internal/descriptor`,
so the prefix doubly misleads. Pre-existing (verbatim-preserved
by the behavior-free move; the codec's tests match on substrings
of these errors, so changing them is a small behavior change that
belongs in its own change set, prefix `"gmdb: "` or
`"descriptor: "` per the package's error convention at that
time).

## Provenance

Adjacent observation from the descriptor-codec-move change-set
review; pre-existing at that change set's base.

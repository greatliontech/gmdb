# No test pins the stable CheckIssue.Code tokens

Lands: when check.go's issue emission or
api-surface.md §CheckIssue is next amended

## Finding

**[L]** `api-surface.md §CheckIssue` declares `Code` "a stable,
machine-parseable token … existing ones never change meaning",
but no test asserts any emitted token string. Demonstrated
consequence: a mechanical rename changed the literal
`"keyspaceDescriptorSize"` to `"descriptor.Size"` in `check.go`
and the full suite stayed green — external tooling matching the
documented token would silently miss the issue class (caught in
review, reverted). Proper pinning needs a per-code reproduction
state (each token requires crafting the corruption that emits
it), which is why this is filed rather than fixed in the chunk
that surfaced it. The token inventory to walk is
api-surface.md's; tests should assert exact `Code` strings on
each crafted state.

## Provenance

Adjacent finding from the descriptor-codec-move change-set
review; pre-existing at that change set's base.

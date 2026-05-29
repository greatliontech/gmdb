# RPL chain "head"/"tail" terminology conflict between chain
# orientation and slice orientation

**Lands:** when the RPL chain or `reclaimRPL` / `trimRPLChainHead` is
next non-trivially touched, or opportunistically as a docs-only
cleanup.

## Problem

`internal/pager/pager.go:369-370` defines the in-memory chain
orientation:

> `// SetRPLChain seeds the in-memory RPL segment list. segments is`
> `// ordered tail (index 0, oldest TxnID) → head (last, newest TxnID).`

So under the chain convention: **tail = index 0 = oldest**, **head =
last index = newest**.

`reclaimRPL` (`internal/pager/freespace.go:418-450`) drains the
oldest segments first — it pops from `p.rplSegments[0]` (the chain
tail). The drain helper is named `trimRPLChainHead`
(`freespace.go:452-462`) and its godoc + inline comment use "head"
to mean "front of the slice / start of the backing array":

- `freespace.go:440-441`: *"copy-trim to free the head slot for GC"*
- `freespace.go:452`: *"trimRPLChainHead removes the consumed head
  entries from the in-memory chain"*
- `freespace.go:456`: function signature

This **conflicts with the `pager.go:370` convention**: per the
chain definition, the helper trims the chain's **tail** (oldest
end), not its head (newest end). A reader following the convention
sees "trim head" and infers "removes the newest entry," which is
the opposite of what the function does.

`docs/specs/transactions.md` §Why this is cheap (the
`rplsegments-clone-cost.md` close-out, this commit) inherited the
prior text's "`reclaimRPL`'s head trim" phrasing. The spec text is
internally consistent (it follows the function's chosen wording)
but propagates the convention conflict into the authoritative
spec.

## Acceptance

One of:

1. **Rename `trimRPLChainHead` → `trimRPLChainTail`** and update its
   godoc / inline comments + the spec's "head trim" reference to
   align with the `pager.go:370` chain convention. Pure rename + 4
   comment-site edits; no behavioral change.
2. **Reverse the chain convention** in `pager.go:370` so index 0 is
   the "head" (oldest) and index N-1 is the "tail" (newest). This
   matches typical FIFO-queue terminology but inverts the existing
   convention. Larger blast radius (any code or comment that uses
   "head"/"tail" of the chain in the existing sense would need
   updating).
3. **Document the conflict inline** in both `pager.go:369-370` and
   `freespace.go:452-455`, explaining that "head" in the function
   name refers to the *slice's* front (the chain's tail), not the
   chain's head. Minimal change; doesn't resolve the conflict but
   surfaces it.

## Notes

Filed at the close-out of `rplsegments-clone-cost.md` by the
adversarial-loop reviewer (M-1, adjacent — pre-existing). Pure
docs/naming cleanup; no correctness implication. Resolution
deferred per the smallest-correct-change escalation rule — this
change set fixed only the `finalizeRPLChain` → `appendRPL`
mis-citation (introduced H-1) and the cross-tx-vs-within-tx
disambiguation (introduced L-1), both narrowly on the causal chain
of the dropped "small constant" preference. A naming convention
clean-up is a distinct concern that did not surface as a second
demonstrated same-fault failing case.

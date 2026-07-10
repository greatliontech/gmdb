# Frozen-mid-publish acquirer residuals need a versioned slot layout

Lands: 12

## Findings

Residuals of the acquire-window hardening (store-time heartbeat,
post-publish ownership verify, re-claim-or-abandon — cross-process.md
§Slot acquire step f), all requiring the acquirer (or the scanner)
frozen/descheduled past a load-bearing window:

**[M] Ghost stores clobber a re-winner's identity fields.** The
resumed acquirer's publish stores (HintEpoch/PIDNamespace/
ProcessStartTime/PID) are plain atomic stores that interleave with
the re-winner's own publish; the verify detects the loss only AFTER
they landed. A clobbered PID/start-time makes the next scan classify
the re-winner by the ghost's identity — if the ghost's process has
exited, the re-winner is evicted (use-after-reclaim), and its own
later release zeroes a slot a third reader may have won (cascading).

**[M] A re-win pinning the SAME TxnID passes the ownership verify —
two owners.** Both the ghost and the re-winner believe they own the
slot; both will Raise/release it. RaiseReaderSlotTxnID's owner-only
precondition is violated the same way in the window between the
verify and restabilization.

**[M] A scanner descheduled between its guard loads and its clear
stores can zero a slot whose frozen occupant resumed and
re-published in between.** The guard's loads and the clear's four
stores are not atomic; the resumed owner's step-f verify can pass
before the clear's final TxnID store lands, leaving it on an
unpinned snapshot (and its later release zeroes a slot a third
reader may have won). Recorded with the other two in
cross-process.md §Slot acquire step f's Accepted residual.

## Fix direction

A per-slot generation word (monotonic, incremented by each successful
acquisition, never zeroed by release/clear) makes ownership checkable:
the verify compares (TxnID, Gen); publish stores become detectable as
foreign via Gen. This is a reader-slot LAYOUT change (48 → 56 bytes)
— land it with the lock-file layout work (boot epoch, shrink
seqlock), which already breaks the lock-file format. Alternative
shapes considered at filing: single-word publication (slots bound to
a registered process identity, acquisition = TxnID store only) — a
larger protocol redesign; per-store TxnID re-checks — shrink but
cannot close the window.

## Provenance

reader-slot-clear-validation remediation: the acquire hardening fixed
the ghost-USE (a resumed ghost proceeding on an unpinned snapshot)
and stale-at-birth-heartbeat aging; these store-level residuals are
what the hardening cannot reach without a layout change. Recorded as
an Accepted residual in cross-process.md §Slot acquire step f.

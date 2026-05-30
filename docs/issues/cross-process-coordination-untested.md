# Cross-process coordination is structurally unverified — no test exercises two live handles on one file

**Lands:** proactive — this is the acceptance gate for
`cross-process-writer-stale-state` and the reader-liveness fixes; it
should land *with* the first of those fixes so the fix has a test that
fails before and passes after.

**Severity:** [H] (verification gap behind a High corruption bug — the
reason that bug went unnoticed)

**Source:** 2026-05-30 deep audit (run `wf_4ad12a2f-039`), raw finding 7.

**Governing spec:** `docs/specs/cross-process.md` (largest spec; the lock
package is ~10 files).

## Problem

The single most complex subsystem in the codebase has **zero end-to-end
coverage at the layer where it is actually consumed**. Every existing
multi-handle test site follows an open→use→`Close`→reopen pattern (e.g.
`bulkload_keyspace_test.go:79`, `check_repair_test.go:128`); none keeps
**two handles live on one file simultaneously** while one observes the
other's commit. This is exactly why the meta-reload corruption
(`cross-process-writer-stale-state`) went unnoticed — there is no test in
which handle B's transaction observes handle A's commit while both are
open. The capability is declared in the Design Decisions table and
specced in detail, but it is not delivered as a working, tested feature.

## Fix

Add a DB-layer cross-handle test matrix, run under `-race`:

- **(a)** Two handles, A commits, B begins-and-reads → B sees A's data.
- **(b)** Two handles interleave writes A,B,A,B → on-disk `TxnID` strictly
  increases and no key is lost.
- **(c)** A long-lived reader on handle B pins a snapshot while handle A
  commits + the writer reclaims → the reader's pages survive (no
  use-after-reclaim).

These tests are the acceptance gate for the meta-reload fix and should be
wired before the multi-process surface is considered complete. Case (c)
also exercises the reader-liveness fixes
(`reader-stale-detection-future-heartbeat-guard`).

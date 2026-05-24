# `setupWriter` test helper duplicated across packages

**Lands:** when chunk 5.3+ adds a second cross-package test that needs
a writer-pager fixture, or earlier if the two helpers drift in a
non-trivial way.

## Problem

`internal/pager/freespace_test.go:17-42` defines `setupWriter` —
the canonical writer-pager fixture (create file, attach bitmap,
seed commit state, mark all data pages free). Chunk 5.2 added the
chunk-4.7 PageWriter parity integration test in
`internal/btree/pager_integration_test.go` which needs the same
fixture but cannot import test-only helpers from `internal/pager`
(test helpers are package-private).

The chunk-5.2 solution was a local duplicate: `setupPagerWriter` at
`internal/btree/pager_integration_test.go:23-51`, with a code-comment
acknowledging the duplication and the trade-off. Acceptable for one
caller; a second caller (chunk 5.3+ keyspace-on-real-pager tests, or
a chunk-7 indexing integration test) would compound the drift risk.

## Acceptance

When the second cross-package writer-pager fixture caller lands,
factor the helper into one of:

1. An exported `pager.NewTestWriter(t, pages int) (*Pager, *bitmap.Bitmap, *os.File)`
   constructor gated by a `_test_helper.go` build tag or named with a
   `Testing` prefix per the project's API surface conventions.
2. A new shared test-helper package `internal/pagertest` (Go's idiomatic
   pattern for cross-package test helpers).

Pick the lower-friction option; either resolves the duplication. The
chunk's chunk-start gate should re-fire this issue if a third caller
shows up.

## Notes

Surfaced by the chunk-5.2 adversarial review (Round 1 L-3). Not a
defect today — the duplication is tracked, justified inline, and the
helpers are simple enough that drift detection is cheap. Filed so it
doesn't get lost when the next caller arrives.

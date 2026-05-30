# Obsolete comment claims indexed-keyspace DeleteRange fallback is unimplemented, but it is wired

**Lands:** proactive — pure documentation rot; trivial.

**Severity:** [L]

**Source:** 2026-05-30 deep audit (run `wf_4ad12a2f-039`), raw finding 18.

## Problem

`keyspace.go:766-769` carries a stale comment claiming the
indexed-keyspace `DeleteRange` fallback is unimplemented ("chunk 7 not yet
implemented"), but it **is** wired (`deleteRangeIndexed`). The code is
correct; only the comment lies. A maintainer reading it would believe
indexed `DeleteRange` is unsupported and might add a redundant guard or
mis-handle the case.

## Fix

Delete/replace the stale comment block with a pointer to
`deleteRangeIndexed` and `docs/specs/range-delete.md §Indexed-keyspace
fallback`. No code change.

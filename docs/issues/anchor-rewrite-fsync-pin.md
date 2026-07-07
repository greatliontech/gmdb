# Test-pin the anchor rewrite in the gated Open's self-durable arm

**Lands:** condition — when an fsync fault-injection seam for the
Open/recovery path exists (e.g. built to test recovery-commit fsync
failure), pin this invariant through it.

## Invariant (documented, enforced by code, not test-pinned)

`pager.RecoverToDurable`'s self-durable arm re-writes the byte-identical
selected meta to its own slot BEFORE the anchoring fdatasync
(`durability.md §Anchoring`). The rewrite is load-bearing: a prior
failed fsync both consumes the kernel's writeback error and marks the
pages clean, so a bare fdatasync on a retried Open would succeed
trivially — anchoring an assertion the disk never received, letting
reclamation free segments a later power loss still needs.

## Why unpinned

A drop-the-rewrite mutation is observable only under fsync fault
injection; the Open path has no such seam (Commit's step-error hooks
do not reach it), and building one solely for this pin was judged
over-encoding. The invariant is encoded at the strongest available
artifacts: the spec clause and the load-bearing code comment naming
the consumed-error failure mode.

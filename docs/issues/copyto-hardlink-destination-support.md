# CopyTo publish requires hard-link support on the destination filesystem

Lands: decided 2026-07-12 — non-hard-link destinations ARE
supported targets; implement the fallback (hard link first, then
no-replace rename: renameat2(RENAME_NOREPLACE) on Linux,
best-effort elsewhere; a failed link with path already naming the
copied inode — the NFS retransmission quirk — is success), with
the api-surface.md destination crash-consistency invariant amended
to the per-filesystem form

## Findings

**[L] The atomic hard-link publish fails unconditionally on
filesystems without hard links.** `copy.go` publishes via
`os.Link(tmp, path)`; `link(2)` returns EPERM/ENOTSUP on vfat/exfat
and many FUSE mounts. The pre-hardening direct-write CopyTo worked
there. A backup target on such a filesystem is plausible even though
the database itself requires POSIX semantics (flock, mmap, fsync).
The failure is loud (an explicit publish error), never silent.

**[nit] NFS link() retransmission quirk.** On NFS, `link()` can
report failure after actually succeeding; that path removes the temp
and returns an error while path holds a complete copy — the only
remaining error-with-copy-present shape (the local dir-fsync-failure
shape was closed by the unpublish).

## Decision needed

Any fallback weakens the contract: `renameat2(RENAME_NOREPLACE)` is
Linux-only; plain rename loses the atomic no-clobber guarantee (and
rename is not guaranteed atomic on FAT); detecting "EPERM because
filesystem" vs "EPERM because permissions" is unreliable. Supporting
these targets therefore means amending api-surface.md's destination
crash-consistency invariant to a weaker per-filesystem form — a
contract decision, not an implementation detail.

## Provenance

Chunk-19 adversarial review of the CopyTo hardening change set.

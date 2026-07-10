# No boot-epoch discriminator: post-reboot liveness misclassification defeats recovery

Lands: 12

## Findings

**[H] After a system crash + reboot, pre-boot heartbeats are "future"
stamps honored as fresh forever, and PID+starttime collisions across
boots pass the identity check — the recovery-commit gate is bypassed
and reclamation is pinned.** Two legs, one root (no boot-epoch
discriminator in the lock file):

(a) `internal/lock/recovery.go:136-141` treats `stamp > now` as fresh
with no upper bound. Heartbeats use a boot-relative clock
(CLOCK_BOOTTIME) and the lock file survives reboot (never deleted), so
after reboot every pre-boot stamp is a huge future value. A
cross-namespace LastWriter record then classifies live,
`PrevLastWriterLive()` is true (`db.go:406`), and the writable Open
takes the live-join branch (`AttachLatest`) instead of
`RecoverToDurable` — attaching the live projection of a SyncLazy meta
whose tree pages the reboot destroyed (page cache lost). The open that
exists to repair instead serves and builds on a partially-flushed tree
until wall-clock outgrows the stamp (days). Pre-boot cross-NS reader
slots likewise pin RPL reclamation for the same duration.

(b) Same-namespace leg: `internal/lock/proc_linux.go` start time is
ticks-since-boot; the init-PID-namespace inode is constant across
boots. A boot-started daemon crashed by power loss can be matched after
reboot by a different boot-started daemon with the same PID and
colliding tick count → identity-live via exact starttime match, no
heartbeat cross-check on that path (`recovery.go:117`). The spec's
"collisions are benign" argument assumes the heartbeat catches it;
post-reboot both defenses fail together.

With page checksums off, garbage is served/committed silently.

**[L] Lock file is never deleted, contra spec.** cross-process.md says
the lock file is "ephemeral (deleted when all processes exit)"; no code
path unlinks it (`lock.go:493-495`) except the stale-UUID arm. Stale
records persisting across reboots is what makes the above reachable
even after clean-adjacent crashes.

## Fix direction

Add a boot-epoch discriminator to the lock-file header (e.g. Linux
`boot_id`), invalidating all stamps and start times from other boots —
this requires amending cross-process.md's future-stamp invariant
(spec-amend rider, surfaced in the audit spec-amend list; the
invariant's darwin/freebsd rationale is also factually wrong — both
clocks are boot-relative kernel-wide — and `mmap_other.go` makes the
lock file Linux-only today). Resolve the lifecycle sentence: either a
deletion protocol or remove the claim (boot-epoch invalidation makes
persistence harmless).

## Provenance

2026-07-10 defect audit; cross-process reviewer. Existing tests pin the
opposite (future⇒fresh as correct); nothing models a clock-epoch reset.

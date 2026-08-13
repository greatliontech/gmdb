# Port: windows

gmdb does not compile for windows today (`GOOS=windows go build`
fails): `internal/lock` calls raw `syscall.Flock` from unguarded
files (`coord.go`, `lock.go`), and `nowMonotonic` has no windows
implementation (`clock_other.go` is tagged `darwin || freebsd`).

## Primitive map

Every platform-touching primitive already sits behind a per-file
seam with a written fallback where one is portable; windows needs:

| Primitive | Today (linux) | Windows analog |
|---|---|---|
| flock (SH/EX/NB) | `syscall.Flock` — in unguarded files (the build break) | `LockFileEx`/`UnlockFileEx`; first move the calls behind the seam |
| mmap (pager RO, lock RW) | `syscall.Mmap` | `CreateFileMapping`/`MapViewOfFile` (bbolt precedent) |
| monotonic clock (cross-process comparable) | `CLOCK_MONOTONIC` | `QueryUnbiasedInterruptTime` (system-wide, excludes suspend — matches the documented suspend caveat) |
| futex wake/wait | linux futex | existing adaptive-poll fallback (`WaitOnAddress` is within-process only, so polling is correct) |
| fdatasync | `fdatasync(2)` | existing `f.Sync()` fallback (`FlushFileBuffers` — full-strength on windows) |
| process start time | `/proc` | `GetProcessTimes` |
| boot id | `/proc/sys/kernel/random/boot_id` | none clean → zero + heartbeat fallback (already the designed degradation) |

## The real risk: filesystem semantics, not primitives

- **Rename over open handles**: every reader holding the DB (or a
  checkpoint/compaction artifact) open without `FILE_SHARE_DELETE`
  blocks the replace path that POSIX rename gives for free. Every
  `os.Open` on the publish/replace path needs the share-mode
  discipline audited; the existing
  `copy_publish_linux.go`/`copy_publish_other.go` split is the seam.
- **Directory-entry durability**: the guarantees pinned by
  `dirent_durability_test.go` need the windows ritual
  (`FlushFileBuffers` on a directory handle opened with write
  access) rather than POSIX dir-fsync.

## Done means soaked

Shims are days; trust requires the crash harness, DST suite, and
cross-process tests green on a windows runner. The
rename/share-mode audit is the part most likely to surface real
divergence.

Lands: when a windows gmdb consumer is first scheduled — first known
candidate: ocifs store bookkeeping on windows (ocifs plans store
bookkeeping on gmdb after its read-path stage, and targets windows
via its ProjFS backend; ocifs may alternatively scope gmdb
bookkeeping to unix first and keep file-based refs on windows — that
choice is made ocifs-side when its store-bookkeeping stage starts).

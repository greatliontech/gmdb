# Port: darwin (macOS)

gmdb cross-compiles for darwin today but cannot open a database at
runtime: both mmap shims are unimplemented off-Linux and both sit on
the unconditional open path (`db.Open` → `lock.Open` → `mmapRW`;
`pager.openPager` → `mmapRO`).

## Nearly free: the shims

The Linux mmap implementations use only portable flags
(`PROT_READ[|PROT_WRITE]`, `MAP_SHARED`; `syscall.Mmap/Mprotect/Munmap`
all exist on darwin), so `internal/pager/mmap_linux.go` and
`internal/lock/mmap_linux.go` port by widening the build tag to the
unix family (matching `proc.go`'s `linux || darwin || freebsd` set,
which `clock_other.go` already tracks). Everything else darwin needs
is already written and correct: futex → adaptive-poll notification
wait, `ProcessStartTime` unavailable → heartbeat liveness fallback,
boot id → zero (cross-boot invalidation disabled), madvise → no-op.

## Not free 1: durability semantics

`internal/pager/fdatasync_other.go` falls back to `f.Sync()` claiming
"fsync is strictly stronger". On darwin that claim inverts for power
loss: Apple documents `fsync(2)` as not flushing the drive cache;
real durability requires `fcntl(F_FULLFSYNC)` (SQLite and LevelDB
both do this). Decision required: unconditional `F_FULLFSYNC` (honest,
slow), an option with a documented default, or a documented weaker
durability tier on darwin. This is the one genuine design call in the
port.

## Not free 2: confidence

The port is done when the crash harness, DST suite, and cross-process
tests run green on a macOS runner (hosted CI has them). Until then
darwin support is "compiles and opens", not "trusted".

## Optional niceties (separate, not blocking)

- `ProcessStartTime` via `sysctl kern.proc.pid` (removes the
  heartbeat-only fallback).
- Boot id via `kern.bootsessionuuid` (restores cross-boot
  invalidation).
- `madvise` hints.

Lands: when a darwin gmdb consumer is first scheduled — first known
candidate: ocifs store bookkeeping on macOS (ocifs plans store
bookkeeping on gmdb after its read-path stage, and targets darwin via
its FSKit backend).

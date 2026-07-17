# Deterministic Simulation Testing (DST)

The DST tier runs gmdb under a deterministic-simulation Go
toolchain fork (`github.com/thegrumpylion/go`, invoked via the
`godst` wrapper): inside `testing/simulation.Run(seed, f)`,
goroutine scheduling, time, runtime randomness, GC, process
identity, the filesystem, and fault injection are a reproducible
function of the seed. One seed = one execution, replayable forever;
sweeping seeds explores interleavings and crash outcomes
systematically.

This spec pins the contract between gmdb and that toolchain: the
build-tag discipline, the simulated syscall surface, the simulation
topology conventions, what each suite enforces, and the seed
policy. It does not restate the simulator's own contract — that is
the fork's `docs/dst/design.md` and `docs/dst/faults.md`.

## Toolchain and invocation

- DST tests build ONLY under the `dst` build tag against the fork
  toolchain, and live in the dedicated `dsttest` package:
  `godst test -tags dst ./dsttest/` (the wrapper exports
  `GOTOOLCHAIN=local` and execs the fork's `go`).
  `simulation.Run` panics in a binary built without the tag.
- Untagged builds (the normal `go` toolchain, normal `go test`)
  carry ZERO DST footprint: every DST test file and every `dst`
  routing sibling is excluded by the tag; production behavior is
  byte-identical to before this spec existed.
- The DST leg is LOCAL-ONLY: a Taskfile `test:dst` target run
  manually against the locally built fork. Merge CI does not gate
  on it. (Revisit when the fork toolchain has a distribution
  story.)

## Invariants

Invariant: kind=clause-explicit;
  property=No fenced syscall is reachable from gmdb production
    code inside a simulation bubble. The fork fences (loud
    deterministic panic) every raw syscall outside its dispatch
    set; gmdb's complete host surface — flock, fdatasync,
    madvise, mprotect, munmap, mmap (MAP_SHARED), clock_gettime,
    pid-liveness probes, futex, renameat2 — is in the fork's
    modeled/dispatched surface (§Simulated syscall surface);
  from=this spec §Simulated syscall surface; the fork's
    interception boundary (its design.md);
  violation=A fenced call reached mid-suite panics the simulated
    process at a schedule-dependent point — every seed sweep
    dies on the fence instead of exploring the behavior the
    suite exists to test.

Invariant: kind=clause-explicit;
  property=A DST suite failure is reproducible from its seed
    alone: suites derive ALL nondeterminism (workload shape,
    fault points, crash choices) from the simulation seed or
    from values pinned in the test, never from host entropy,
    host time, or unseeded `math/rand`;
  from=this spec §Seed policy;
  violation=A failing sweep seed that does not replay — the
    single property the tier exists to provide is silently lost,
    and a real bug found once is unfindable again.

## Simulated syscall surface

gmdb production code runs UNMODIFIED under simulation — no
build-tag routing of production paths. The fork models gmdb's
complete Linux host surface, including the two calls it did not
model when this tier was first scoped (tracked and fixed in the
fork per its own contract, `docs/dst/design.md` there):

- **`SYS_FUTEX`** (`FUTEX_WAIT` with timeout / `FUTEX_WAKE`, the
  shared non-PRIVATE form on `MAP_SHARED` file pages) — the
  notification waiter of cross-process.md §Notification region
  runs its real Linux path in-simulation.
- **`SYS_RENAMEAT2`** (`RENAME_NOREPLACE`) — CopyTo's atomic
  no-replace publish rung (api-surface.md §Check, CopyTo,
  Compact) runs its real Linux path in-simulation.

Known modeled-surface residuals:

- **`link(2)` is not modeled**: CopyTo's preferred hard-link
  publish arm (and the NFS link-retransmission quirk detection)
  is unreachable in-simulation — every simulated CopyTo publishes
  through the renameat2 no-replace rung. The link arm and the
  quirk are host-only paths; suites exercising the publish
  exercise the rename rung.
- **`/proc/sys/kernel/random/boot_id`** is not modeled; the read
  fails and gmdb runs with the ZERO boot epoch — cross-boot
  invalidation disabled, exactly the spec'd degradation for
  unreadable-/proc environments (cross-process.md boot-epoch
  clauses). Suites therefore cannot exercise the boot-epoch reset
  path until the fork models per-boot host identity — the suite's
  landing condition, stated here in full, not silently skipped.

## Simulation topology conventions

- One simulated **Host** per database fixture: co-located
  simulation **Processes** on that host share the filesystem
  tree, flock namespace, and the lock file's MAP_SHARED page
  cache — the fork's model of gmdb's multi-process deployment.
- Each simulated gmdb process (a writer, a reader, a contending
  opener) runs inside its own `Process` node and communicates
  with its peers ONLY via the shared host filesystem — never via
  Go channels, shared Go memory, or `sync` primitives crossing
  node boundaries (the fork's crash model is sound only under
  this discipline; a crashed node's goroutines never unwind).
- Process crash (`Crash`) models `kill -9`: the page cache
  survives, un-fdatasync'd writes remain visible to peers and
  restarts. Host crash (`CrashHost`) models power loss: only the
  fdatasync'd durable image survives, optionally page-torn
  (`CrashTear: true` explores the crash-consistent outcome set).
  Suites choose the fault matching the failure they pin.

## Suites

Each suite is a spec-coverage walk, not a smoke test: it names
the spec sections whose invariants it enforces, and asserts them
across seeds.

- **Coordination suite** — cross-process.md: writer-grant
  mutual exclusion and handoff under contention; stale-writer
  takeover after `Crash("writer")` and the recovery-commit gate's
  dead-vs-live author classification (the PID-LIVENESS leg; the
  start-time and heartbeat legs need pid reuse and namespace
  divergence, which the fork's identity model — monotonic
  never-reused pids, a single namespace — cannot express: they
  land under a `Lands:` condition, see the issue index); reader-slot
  acquisition, reaping
  of crashed readers, snapshot pinning across a writer's
  commits; change notification (`Version`/`WaitVersion`/
  `WaitKeyspaceVersion`) over the real futex waiter, including
  no-lost-wake across publish and cancellation.
- **Crash suite** — durability.md: acked-durable commits always
  recover byte-exact after `CrashHost` at any point; an
  in-flight commit preserves the prior epoch under `CrashTear`
  wreckage (torn meta included); SyncLazy/SyncDataOnly recover
  to their spec'd epochs; recovery leaves `Check` clean. The
  existing `crashRecorder` harness (FileOps-seam image
  synthesis) REMAINS the fast untagged unit tier — the DST suite
  adds what it cannot reach: lock-file state, directory entries,
  and the CopyTo/Compact publish inside the crash image.
- **Fault suite** — durability.md §Commit Outcome
  Classification + api-surface.md publish contracts under the
  fork's disk faults: fsyncgate (an EIO'd barrier drops dirty
  pages; the retried barrier must not be trusted), ENOSPC
  mid-commit, disk latency; clock step/drift against heartbeat
  and stale-detection logic.
- **Exploration tier** — `Explore`/PCT over the hottest
  interleaving surfaces (commit vs concurrent readers vs the
  maintenance daemon vs notification waiters), with failures
  replayed via `Replay`.

## Seed policy

- Every suite runs a small set of PINNED anchor seeds (committed
  in the test source — regressions reproduce identically
  everywhere) plus a sweep of `DST_SEEDS` additional seeds
  (environment variable; default small, cranked up for long
  local runs).
- A failure report always includes the seed; fixing a
  seed-found bug adds that seed to the suite's anchors.

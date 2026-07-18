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
complete Linux host surface (each capability tracked and landed in
the fork per its own contract, `docs/dst/design.md` there):

- **`SYS_FUTEX`** (`FUTEX_WAIT` with timeout / `FUTEX_WAKE`, the
  shared non-PRIVATE form on `MAP_SHARED` file pages) — the
  notification waiter of cross-process.md §Notification region
  runs its real Linux path in-simulation.
- **`SYS_RENAMEAT2`** (`RENAME_NOREPLACE`) — dispatched by the
  fork. Because the fork also models `link(2)`, CopyTo's publish
  ladder takes its PREFERRED hard-link arm in-simulation and the
  renameat2 rung is unreachable through CopyTo there (a modeled
  filesystem supports hard links); the rung's coverage lives in
  the untagged unit tier's publish-seam tests and the fork's own
  raw-dispatch tests — stated, not silently capped.
- **`link(2)`** — CopyTo's hard-link publish arm runs its real
  Linux path in-simulation. The NFS link-retransmission quirk
  remains a host-only shape (the modeled filesystem loses no
  replies); the quirk detection stays unit-tier-covered.
- **`/proc/sys/kernel/random/boot_id`** — the fork serves a
  deterministic per-boot epoch (regenerated across
  CrashHost + Host re-declaration), so cross-boot invalidation is
  ACTIVE in-simulation and the boot-epoch suite exercises the
  reset end-to-end.
- **Process identity divergence** — pid reuse (the fork's
  `Options.PidMax` pid_max model) and sibling pid namespaces
  (`ProcessWith`) are constructible, so the stale-identity
  start-time and cross-namespace heartbeat legs run end-to-end.

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
  dead-vs-live author classification; all THREE stale-identity
  legs walked end-to-end on the READER-SLOT path — pid liveness,
  start-time discrimination against a live pid-reuse impostor
  (`Options.PidMax`), and the cross-namespace heartbeat window
  against a sibling-namespace crash (`ProcessWith`); the
  writer/last-writer record consumers reach the same
  classification through the one shared classifier
  (cross-process.md's stale-detection rules), pinned at the unit
  tier; reader-slot
  acquisition, reaping
  of crashed readers, snapshot pinning across a writer's
  commits; change notification (`Version`/`WaitVersion`/
  `WaitKeyspaceVersion`) over the real futex waiter, including
  no-lost-wake across publish and cancellation.
- **Boot-epoch suite** — cross-process.md §BootID: cross-boot
  invalidation across `CrashHost` + `Host` re-declaration, with
  the wedged resource constructed so the adoption reset is the
  only possible recovery (a cross-namespace slot whose heartbeat
  reads as the new boot's future).
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
  fork's disk and clock faults, in five legs:
  - *Fsyncgate sweep* under BOTH real disk-failure shapes: the
    writeback fault (`FailWriteback` — cache-served buffered I/O,
    EIO'd barriers drop dirty pages) walks the three-class
    certainty statement end-to-end — every post-usage-check
    commit failure must carry a class, since the verification
    preads are cache-served — plus recovery and power-loss
    verification per class; the whole-stack fault (`FailDisk`,
    one anchor) pins the documented unclassified fallback (the
    EIO'd verification read withholds the class; "do not retry;
    re-Open and probe"). The recovery gate's bitmap redirty
    (durability.md §Anchoring) is enforced here.
  - *ENOSPC creation paths* (CopyTo / create-Open on a full
    disk: clean classified failures, no wreckage; freeing space
    restores). Mid-commit data ENOSPC is unconstructible
    in-simulation for a truncate-growing store under the fork's
    logical-bytes model (its recorded boundary): that
    classification stays pinned by the untagged FileOps-seam
    tier — stated, not silently capped.
  - *Disk latency*: a commit stretched past StaleTimeout by a
    slow disk must not be falsely reaped as stale.
  - *Bit rot*: platter corruption surfacing at reboot is
    detected (open error, Check issue, or read error) — never
    silently served.
  - *Wall-clock immunity*: step and drift faults never perturb
    the BOOTTIME-based heartbeat/stale-detection protocol.
- **Exploration tier** — systematic interleaving exploration over
  the hottest surfaces: commit vs concurrent readers (snapshot
  never torn), commit vs the notification waiter (WaitVersion bounded by a
  VIRTUAL-time context so a lost wake surfaces as a deterministic,
  replayable violation — an unbounded park would churn heartbeat
  timers against the wedge detector instead of failing crisply,
  and the bound costs nothing when the wake arrives), commit vs the maintenance daemon enabled at a short
  interval (every schedule Check-clean), plus a PCT
  (priority-inversion) sweep of the reader surface. The suite
  explores in DPOR mode in EVERY build under an explicit per-seed
  schedule budget, reported never silently capped: the fork's
  COARSE cross-process dependency model (its exploration.md —
  file nodes, the host namespace, flock, the shared futex,
  announced build-independently) makes non-race DPOR genuinely
  explore and prune gmdb's multi-process conflicts at OS-object
  granularity — pinned in-suite by the coarse-visibility test
  (gmdb SUTs must never report `Uninstrumented`, and every found
  failure must replay, with panic/deadlock-typed failures
  reproducing by panic per the fork's Replay contract). The
  dst-race leg (`test:dst:race`) adds memory-granularity
  dependencies on top, with a RECORDED scale boundary: a single
  full-database run carries enough instrumented accesses to
  overflow the explorer's per-bubble access budget, so the
  full-stack legs report `Overflow` after one schedule — stated,
  never silently capped; memory-granular pruning bites at
  component-scale SUTs (the workflow self-test explores and
  prunes end-to-end under both builds). The
  Explore → Failure → Replay workflow itself is proven end-to-end
  on a known lost-update SUT before any real bug needs it. Every
  failure report carries the seed and the replayable schedule.

## Seed policy

- Every suite runs a small set of PINNED anchor seeds (committed
  in the test source — regressions reproduce identically
  everywhere) plus a sweep of `DST_SEEDS` additional seeds
  (environment variable; default small, cranked up for long
  local runs). `DST_SEEDS` accepts either `+N` — a COUNT appending N
  consecutive seeds from a fixed base far above every anchor, so
  extras are deterministic, reportable, and promotable — or a
  comma-separated list of explicit seed values, including a single
  one (re-running or bisecting a reported seed). The `+` prefix is
  what disambiguates: a bare number is always a seed VALUE, never
  a count, so a reported seed can be pasted verbatim. Anything
  else fails loud. The extension is logged, so a sweep's coverage
  is always stated (the `sweepSeeds` helper).
- A failure report always includes the seed; fixing a
  seed-found bug adds that seed to the suite's anchors.

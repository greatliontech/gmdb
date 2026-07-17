# Plan: DST testing tier

Spec: docs/specs/dst-testing.md

Fork prerequisites — landed (fork branch `dst`): renameat2
dispatch and the shared-futex model, each with its own reviewed
plan in the fork repo.

- [x] 1. Toolchain wiring + fence probe: Taskfile `test:dst` leg
      (godst, `-tags dst`); tagged smoke test — `simulation.Run` →
      Open → write → commit → reopen → `Check` — confirming gmdb's
      full production path (real futex waiter; the publish arm the
      modeled surface selects) runs fence-free, and that identity/clock/boot-epoch surfaces
      behave as the spec's §Simulated syscall surface records.
- [x] 2. Coordination suite: multi-process Host/Process topology
      helpers + cross-process.md walk (grant handoff, stale-writer
      takeover, reader-slot reaping, snapshot pinning, notification
      over the real futex waiter).
- [x] 3. Crash suite: CrashHost/CrashTear seed sweeps over
      durability.md (acked-durable recovery, in-flight-commit epoch
      preservation, SyncLazy/SyncDataOnly epochs, publish-path crash
      images).
- [x] 4. Identity + boot-epoch suites (unlocked by the fork's per-boot
      host identity and process-identity models): boot-epoch reset
      end-to-end across CrashHost+reboot; stale-identity start-time
      (pid-reuse) and cross-namespace heartbeat legs; spec residuals
      refreshed; CurrentBootID per-call read (the per-process cache's
      safety assumption fails when one OS process spans boots).
- [ ] 5. Fault suite: fsyncgate, ENOSPC mid-commit, disk latency,
      clock step/drift vs heartbeat and stale detection.
- [ ] 6. Exploration tier: Explore/PCT interleaving runs, Replay
      workflow, seed-policy plumbing (`DST_SEEDS`, anchor seeds).

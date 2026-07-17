# DST coordination suite: start-time and heartbeat stale-writer legs

Lands: when the DST fork models pid reuse and/or pid-namespace
divergence (tracked in its issue index as "process-identity
divergence modeling").

## Gap

cross-process.md's stale-writer detection has three legs: pid
liveness (kill(pid,0)), start-time discrimination (a REUSED pid with
a different /proc start-time), and heartbeat age (cross-namespace,
where pid probes are meaningless). The DST coordination suite pins
only the pid-liveness leg (TestSimulationStaleWriterTakeover,
TestSimulationRecoveryGateDeadVsLiveAuthor): the fork allocates pids
monotonically, never reused within a run, and serves the constant
namespace pid:[1] to every simulated process, so the states the
other two legs classify are structurally unconstructible
in-simulation. The untagged unit tier (internal/lock tests) covers
their logic in-process; the cross-process end-to-end walk waits on
fork capability.

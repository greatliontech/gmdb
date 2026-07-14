# No cross-process change notification; consumers poll

A reader that wants to react to writes from another process has no
primitive to block on: the lagging-reader callback concerns reclaim
policy, and the deferred-notification pattern is in-process. A
substrate-watching consumer (a session picker refreshing while other
processes register work; any watch-shaped reader over shared state)
polls the root version.

Fix shape: `Version() uint64` plus a blocking
`WaitVersion(ctx, from uint64) (uint64, error)` that returns when
the committed-visible root version exceeds `from`, and a
keyspace-scoped `WaitKeyspaceVersion(ctx, name, from)`; spurious
wakeups allowed, callers re-check. The notification region in the
shared lock file (where PID heartbeats already live) is a fixed
array of counter words: slot 0 bumps on every commit (global wait),
slots 1..K bump per touched keyspace by name-hash — so scoped
waiters are not woken by unrelated-keyspace commits, and a hash
collision is just a spurious wake. Futex wait on the slot word on
Linux, adaptive poll over the mmap'd words as the portable
fallback; cross-namespace fallback stays poll. Futex-first because
the triggering consumer — gitfs's cross-runtime change-propagation
channel — carries a sub-millisecond wake contract that an adaptive
poll's backoff ceiling cannot honor.

Lands: 14 (`docs/plans/pre-consumer-engine-changes.md`). (weaver's
substrate-watching surfaces — session pickers, unowned-state
listings — remain follow-on consumers; live streams reach their
serving process directly and never need this.)

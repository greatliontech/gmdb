# No cross-process change notification; consumers poll

A reader that wants to react to writes from another process has no
primitive to block on: the lagging-reader callback concerns reclaim
policy, and the deferred-notification pattern is in-process. A
substrate-watching consumer (a session picker refreshing while other
processes register work; any watch-shaped reader over shared state)
polls the root version.

Fix shape: a blocking `WaitVersion(from uint64, ctx) (uint64, error)`
that returns when the committed root version exceeds `from`.
Implementable first as an internal adaptive poll over the mmap'd meta
(cheap — one cached read per tick), upgradeable to a futex word in the
shared lock-file region (where PID heartbeats already live) with no
API change. Cross-namespace fallback stays poll.

Lands: when a consumer's poll cadence becomes a real cost or latency
bound (weaver's substrate-watching surfaces — session pickers,
unowned-state listings — are the nearest consumers; live streams
reach their serving process directly and never need this).

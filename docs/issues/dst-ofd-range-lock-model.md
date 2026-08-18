# DST: model OFD byte-range locks in the godst fork

**Lands: user decision** (work lives in the godst fork toolchain, a
repo outside gmdb; scheduling and authorization are the user's).

## Gap

The reader-slot liveness rework put Linux slot locks on OFD
byte-range locks (`fcntl(F_OFD_SETLK)` over each slot's 56 bytes;
`docs/specs/cross-process.md` §Reader Table, slot locks). The godst
fork models `flock(2)` — including crash release — but not OFD
range locks, so the simulated syscall boundary refuses the fcntl
and every simulated `BeginRead` would panic.

Interim resolution (see `docs/specs/dst-testing.md` §Simulated
syscall surface, reader-slot locks bullet — the one sanctioned
build-tag routing exception): under the `dst` tag
`flock.RangeSupported` is false and slot locks run the per-slot
lock-FILE backend, the same portable tier darwin/freebsd run in
production. The slot protocol above the backend seam is fully
explored in-simulation; the OFD backend itself is real-kernel
unit-tier coverage only.

## What landing looks like

In the fork (per its `docs/dst/design.md` capability contract): an
OFD lock table keyed by open file description, per-description
conflict semantics (a description never conflicts with itself),
byte-range overlap checks, `F_OFD_SETLK` try/unlock dispatch at
the named-wrapper and raw boundaries, and release on process crash
and description close. In gmdb: delete the `dst` leg from
`internal/flock/range_linux.go` / `range_other.go` build tags and
the spec's routing-exception bullet, then rerun `task test:dst` —
the simulation then explores the Linux production backend
directly.

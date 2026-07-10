# Stale-reader clear without occupancy re-validation evicts live readers

Lands: 10

## Findings

**[H] The stale-reader scan classifies a slot from fields loaded at
separate instants, then clears unconditionally — a live reader that
re-won the just-freed slot is evicted.** `internal/lock/reader.go:261-266`
load `TxnID`, `PID`, `Heartbeat` at separate instants;
`reader.go:321-323` load `ProcessStartTime`/`PIDNamespace` later;
classification runs syscalls (`kill(2)`, `/proc` reads); finally
`ClearStaleReaderSlot` (`reader.go:172`) stores unconditionally — no
re-check that the slot still holds the observed `(TxnID, PID)`.
Interleaving (reader acquire/release is lock-free; the writer's flock
excludes nothing here): scanner loads R1's coherent fields → R1
releases and exits (short-lived client) → R3 CAS-wins the freed slot
(the slot-hint makes this the *likely* reuse) and fully publishes →
scanner's liveness check on the departed PID fails → clear zeroes R3's
live slot. The reclamation bound (`db.go:1075`) advances past R3's
snapshot; RPL reclamation frees pages R3 is reading (use-after-reclaim,
torn snapshot); R3's own later release then zeroes a slot a fourth
reader may have won (cascading eviction). Window spans two syscalls
plus preemption — µs to ms.

**[M] Reader-slot heartbeat is stamped with a clock value read before
the scan; a process frozen ≥ StaleTimeout inside the acquire window
resumes into an evicted slot and ghost-stores over the next owner.**
`internal/lock/coord_reader.go:58` reads `now` once; `reader.go:83`
stores it after the CAS. SIGSTOP/cgroup-freeze between CAS and
heartbeat store for > StaleTimeout lets a LOCK_EX scan clear the slot
via case 0b/0c (namespace-blind, so same-host readers are exposed); the
resumed reader finishes its `PIDNS/PST/PID` stores into a freed
(possibly re-won) slot — PID is stored last, so the ghost's PID can
overwrite the new owner's published identity — and proceeds on an
unpinned snapshot. Case 0c's aging is partially spec-accepted; the
ghost-store corruption of the next owner is not.

**[M] DB.Close munmaps the lock file while a straggling ReadTx
release still dereferences it — data race, potential SIGSEGV on the
unmapped slots array.** `db.go:706` (step 8 `lockFile.Close()` nils
`f.mmap`/`f.slots`, `internal/lock/lock.go:506`) races
`ReadTx.close` → `Coord.ReleaseReader` → `File.ReleaseReaderSlot`
(`internal/lock/reader.go:115` reads `f.slots`). The step-8 comment
claims `closeGate.BeginClose` drained Tx cleanups, but the drain does
not cover a ReadTx that lost the BeginRead/Close race and is running
its release path. Demonstrated: `-race` fires on
`TestBeginReadCloseRaceReturnsErrClosed` on HEAD (timing-sensitive:
1 hit in 20 standalone `-count` runs in one session, 0 in 30 in
another; also observed under full-suite contention). Best case the racing release panics on the nil `slots`
guard; worst case it stores through the stale mmap pointer after
munmap — SIGSEGV, or a write landing in whatever the address space
reuses. Found by the pager-commit-residue chunk's full-suite run
(adjacent — reproduces on base).

**[L] False soundness claim guarding the recovery gate.**
`internal/lock/coord_reader.go:156-158` states the gate "holds
flock(LOCK_EX) (no acquire/release can race the scan)" — false: reader
acquisition never takes flock. The outcome is covered by durability.md's
unrecovered-window contract, but the stated justification would mislead
any change built on it.

## Fix direction

Make the clear conditional on the classified occupant: re-validate
`(TxnID, PID)` (or CAS the clear against the observed identity) so a
re-won slot is never zeroed; stamp the acquire heartbeat from a clock
read at store time and define a publish-completion check so a resumed
ghost cannot finish publishing into a cleared slot (e.g. acquirer
re-verifies slot ownership after the final store, releasing if lost).
Scanner-side snapshot-coherence clause lands in cross-process.md
§Stale-reader detection (spec-amend rider, surfaced in the audit
spec-amend list). Correct the coord_reader comment.

## Provenance

2026-07-10 defect audit; cross-process reviewer. reader_test.go pins
classifications on quiescent tables and acquirer-side orderings; none
interleave a scan with slot churn.

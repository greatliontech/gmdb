# oslock — advisory file locks as liveness witnesses

The public `oslock` package: exclusive advisory file locks with
process-lifetime binding, for callers that use held locks as
cross-process liveness verdicts (a claim is alive exactly while its
holder keeps the lock; the kernel releases at process death,
SIGKILL included). gmdb's own database coordination does not use
this package's file layout — it shares only the platform flock seam
(`cross-process.md` §Write Lock); this spec governs the public
surface.

Scope:
- The Lock type and its lifecycle (acquire, then Retire — the claim
  ends, unlink-while-held then release — or Close — release only,
  the claim's name persists), and Path.
- TryAcquire: non-blocking three-valued verdict-and-claim; ErrHeld;
  undecided errors.
- Acquire: poll-based, context-cancellable acquisition.
- The identity-verified acquire and the retirement discipline;
  ErrUnlinkDeferred and ErrRetired.
- The locking-domain soundness boundary.

## Contract

A `Lock` is one open file description holding the exclusive
advisory lock on a lock file, constructed only by a successful
acquisition. Distinct open file descriptions exclude each other
in-process exactly as across processes. Descriptors are
close-on-exec and never placed on a child's inheritance list by the
package; a fork that never execs shares the description until it
exits — a bounded false-live in the safe direction.

The verdict is three-valued. `TryAcquire` never blocks: `ErrHeld`
is the verdict that a live holder exists; success is simultaneously
the verdict that no holder survives and the caller's own claim —
one atomic act, so no verdict can go stale between judging and
acting; every other error wraps `ErrUndecided` — the try could not
judge (a transient open failure, a permission problem, a churned
path) and the caller retries later, never treating it as death, so
verdict consumers branch on exactly three named outcomes. `Acquire`
waits by polling a non-blocking probe — never a blocking lock call:
a cancelled wait leaves zero goroutines, zero descriptors, and zero
abandoned kernel waiters behind (the accumulation pathology
`cross-process.md` §Write Lock rejects), and acquisition follows a
release within one poll interval. A transient open failure is
retried against a bounded per-call budget (on the order of 100ms);
a persistent one (permissions, a missing parent) surfaces as its
error rather than polling until cancellation.

Acquisition verifies identity after locking: the locked
descriptor's file compared against the path's; a mismatch (the file
was unlinked and recreated underfoot) closes and retries on the
path's current file — in `Acquire` paced by the poll interval and
cancellable (an uncancellable context never turns churn into a
busy-spin), and in `TryAcquire` against a bounded budget on the
order of a hundred retries — tolerant of any benign race, exhausted
only by sustained foreign churn — whose exhaustion surfaces as
`ErrUndecided`, never a block and never a death verdict. A claim
whose meaning has ended is retired with `Retire`: the lock file is
unlinked while the lock is still held, and only then released —
unlink-after-release would race a fresh acquirer of the old inode
into a second-holder state the identity verify cannot catch.
`Close` alone releases without unlinking (the claim's name persists
for the next acquirer), and a closed Lock's `Retire` is refused — a
retired descriptor must never unlink a successor's live lock file.
A platform refusing to unlink open files defers the unlink (the
release still happens) — the lock, not the file's absence, is the
authority, and an unheld leftover file is an acquirable dead claim,
not a hazard.

Verdicts are sound only within one locking domain — the set of
openers among whom the filesystem makes advisory locks conflict.
One host's processes opening the same filesystem are one domain
whatever their namespaces; a network filesystem with client-local
locks, or a stacked view (overlay upper, passthrough FUSE), is a
different domain and yields no sound verdict. The package cannot
probe the domain; callers for whom a false-dead verdict is
destructive probe at startup (hold, then try-acquire through a
second descriptor; the try must observe contention).

## Invariants

Invariant: kind=clause-explicit;
  property=A successful TryAcquire proves the previous holder's
    open file descriptions are all closed (process death included),
    and the success itself is the caller's claim.
  from=this spec §Contract;
  violation=A verdict-then-act gap (judge dead, then separately
    claim) lets a racing successor claim between the two steps —
    two actors both proceed on "I judged it dead".
  Enforced by `TestCrossProcessVerdicts` and
    `TestTryAcquireVerdicts`.

Invariant: kind=clause-explicit;
  property=A Lock is only ever constructed on a descriptor whose
    file the path still names at acquisition time.
  from=this spec §Contract (identity-verified acquire);
  violation=Locking an unlinked inode while a recreated file sits
    at the path grants two simultaneous "holders" of one claim
    name — both pass verdicts, both act.
  Enforced by `TestAcquireRetriesUnlinkRecreateRace` (the loop
    wiring, via the between-lock-and-verify test seam) and
    `TestVerifiedCatchesUnlinkRecreate` (the comparison itself).

Invariant: kind=clause-explicit;
  property=A cancelled Acquire leaves nothing behind that can ever
    take the lock — no goroutine, no descriptor, no queued kernel
    waiter.
  from=this spec §Contract;
  violation=An abandoned waiter granted the lock after
    cancellation holds a claim its caller believes was never made —
    the claim leaks until process exit and every verdict on it
    reads live; accumulated abandoned waiters exhaust descriptors
    and threads.
  Enforced by `TestAcquireCancel` (behavior); the poll design makes
    the waiter class structurally absent.

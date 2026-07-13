# Durability Modes

Three sync modes (`SyncDurable`, `SyncDataOnly`, `SyncLazy`). The
mode controls which `fdatasync()` calls fire during commit and what
state recovery adopts after a crash.

Scope:
- `Options.SyncMode` semantics.
- The durable sub-record on the meta page and `DB.Checkpoint()`
  mechanics.
- Recovery rules (which state is adopted).
- Cross-process `SyncMode` interleaving.

Depends on / interacts with:
- `pager-slab.md` for commit step 2 / step 4 fdatasync placement.
- `file-layout.md` for the meta page's durable sub-record fields.
- `free-space.md` for the durable-epoch bound used by RPL
  reclamation in `SyncLazy`.
- `api-surface.md` for `SyncMode` constants and `Checkpoint`.

## Invariants

Invariant: kind=clause-explicit;
  property=`SyncDurable` issues `fdatasync` at both commit
    step 2 (data + RPL + bitmap) and commit step 4 (meta). After
    `SyncDurable`'s commit returns successfully, the commit is
    durable end-to-end;
  from=this spec §Durability Modes table + `pager-slab.md`;
  violation=A `SyncDurable` commit that returns success without
    durable meta-fsync violates the user-facing ACID contract —
    an ack'd transaction can be lost on crash, the worst-case
    surprise.

Invariant: kind=clause-explicit;
  property=Recovery selects the highest-`TxnID` valid meta — the
    same selection live operation uses — and adopts its **durable
    sub-record** (the durable projection), never the selected
    meta's live tree;
  from=this spec §Recovery;
  violation=Adopting a live tree whose data pages were never
    fsynced (a `SyncLazy` commit's) lets readers traverse pages
    the OS never flushed, surfacing as `ErrBadPageChecksum` or
    wrong values; adopting a stale slot when a newer valid one
    exists silently loses durable commits.

Invariant: kind=clause-explicit;
  property=`DurableTxnID` and `AnchoredDurableTxnID` are
    monotonically non-decreasing across the commit sequence, and
    `AnchoredDurableTxnID <= DurableTxnID`: every commit carries
    the previous meta's sub-record forward unchanged, or replaces
    `DurableTxnID` with its own (strictly newer) state when its
    data is confirmed durable, and advances
    `AnchoredDurableTxnID` only to an assertion a completed
    fdatasync has covered (§Anchoring);
  from=this spec §Checkpoints, §Anchoring;
  violation=A sub-record that retreats lets the latest meta name
    an older durable epoch than an earlier meta did — recovery
    and the RPL reclamation bound (`free-space.md`) would trust a
    bound newer segments were already reclaimed against, freeing
    pages the recovered tree references.

Invariant: kind=clause-explicit;
  property=The RPL reclamation bound never exceeds the **anchored
    epoch** — the newest `DurableTxnID` assertion whose carrying
    meta pwrite a completed fdatasync has covered (§Anchoring) —
    so the bound never exceeds any epoch a crash at any instant
    could make recovery adopt;
  from=this spec §Anchoring + `free-space.md §RPL Reclamation`;
  violation=Trusting an UNANCHORED assertion (a `SyncDataOnly`
    commit's self-durable meta, pwritten after its data fsync but
    itself never fsynced) lets reclamation free pages by a bound
    the disk does not yet record: the OS drops that meta in a
    crash, recovery adopts the older on-disk epoch, and its tree
    references pages reclamation already handed out — silent
    corruption on a mode composition this spec guarantees.

Invariant: kind=entailed;
  property=`DB.Checkpoint()` makes prior `SyncLazy` commits
    durable: after Checkpoint returns success, every commit whose
    `TxnID <= active meta TxnID at Checkpoint time` is on stable
    storage, and the active meta's durable sub-record names its
    own live state (`DurableTxnID == TxnID`);
  from=entailed: §Checkpoints mechanics (fdatasync at step 2 +
    sub-record bump + fdatasync at step 4);
  violation=A "successful" `Checkpoint` that fails to fdatasync
    prior pwrites lets recovery adopt a sub-record naming
    not-yet-flushed pages — silent corruption.

Invariant: kind=entailed;
  property=Multiple processes attached to the same database may
    use different `SyncMode`s; recovery composes correctly
    because the durable sub-record reflects the last fsync-ing
    event by ANY process (carried forward by every committer),
    and recovery adopts the highest-`TxnID` valid meta's
    sub-record regardless of which process wrote it;
  from=entailed: §Cross-process SyncMode interleaving;
  violation=An assumption that all processes share a `SyncMode`
    fails under mixed deployments (e.g., a `SyncLazy` build of
    one binary alongside a `SyncDurable` build of another) —
    correctness must hold across the composition.

## Durability Modes

Three modes, configurable via `Options.SyncMode`. The mode
controls which `fdatasync()` calls are performed during commit.
All modes preserve **database integrity** (the file is always
structurally valid).

| Mode | Data Sync | Meta Sync | On Crash | Performance |
|------|-----------|-----------|----------|-------------|
| `SyncDurable` (default) | `fdatasync()` | `fdatasync()` | No data loss. Full ACID. | Slowest |
| `SyncDataOnly` | `fdatasync()` | skip | The commits since the last durable-epoch advance may be lost (in pure `SyncDataOnly` use: at most the last one). DB is consistent — falls back to the surviving meta's sub-record. | ~2× faster |
| `SyncLazy` | skip | skip | Rolls back to the **durable epoch** (the last fsync point). DB is always consistent — no corruption. | Much faster |

## Checkpoints and the durable sub-record

In `SyncLazy` mode, commits pwrite bitmap, data, and meta but
skip all `fdatasync()` calls. The OS page cache holds the
writes; order is not guaranteed.

The **durable epoch** is the newest `TxnID` whose data pages have
been confirmed on stable storage. The epoch advances when:

- `DB.Checkpoint()` is called explicitly (`fdatasync` of the
  data file).
- A commit happens in `SyncDurable` or `SyncDataOnly` mode (these
  sync data pages as part of their normal commit path).

Each meta page carries a **durable sub-record** (`file-layout.md
§Meta Page`): the durable epoch's `TxnID` plus the state-bearing
fields of that epoch's meta (keyspace root and count,
HighWaterMark, RPL head/tail/entry-count/head-TxnID, free-page
count) — everything recovery needs to adopt that epoch's tree.
A commit whose own data is confirmed durable (SyncDurable /
SyncDataOnly step-2 fsync) writes its meta with the sub-record
naming ITSELF (`DurableTxnID == TxnID`). A `SyncLazy` commit
carries the previous meta's sub-record forward unchanged.
`DB.Checkpoint()` re-writes the active meta with the sub-record
bumped to the meta's own live state. The file-format fields
(`MinSize`, `GrowStep`, `ShrinkThreshold`) are deliberately NOT
part of the sub-record: they are policy, safe to pair with any
tree, and recovery adopts the selected meta's live values.

### Anchoring — when an epoch assertion may bound reclamation

A `DurableTxnID` assertion protects recovery only once it is
itself on stable storage. A `SyncDataOnly` commit writes a
self-durable meta (its data was fsynced at step 2) but never
fsyncs that meta — until a later fdatasync covers the meta
pwrite, a crash can drop the assertion while keeping everything
reclamation did under it. The **anchored epoch**
(`AnchoredDurableTxnID`) is therefore tracked alongside the
sub-record: the newest `DurableTxnID` assertion whose carrying
meta pwrite preceded a completed fdatasync of the file.

- Every fdatasync that COMPLETES anchors every assertion pwritten
  before it: a `SyncDurable` commit's step-4 anchors its own
  assertion; `Checkpoint()`'s step-4 anchors its bump; a
  `SyncDataOnly` commit's step-2 anchors everything already
  pwritten (in pure `SyncDataOnly` use the anchored epoch trails
  the durable epoch by exactly one commit).
- A `SyncLazy` commit fsyncs nothing and anchors nothing.
- **The persisted field never runs ahead of a completed fsync.**
  `AnchoredDurableTxnID` as pwritten names only an assertion whose
  covering fsync had ALREADY returned when the meta was written —
  a `SyncDurable` commit persists the pre-commit anchored value
  and advances its in-process knowledge to its own `TxnID` only
  after step 4 returns (the next meta persists it). A
  forward-promise ("the fsync I am about to run") is exploitable:
  if that fsync FAILS, the handle poisons but the pwritten claim
  stays live-visible in the shared page cache, and a peer's
  re-sync would bound reclamation by an assertion the disk never
  received while the kernel's consumed error lets the data
  overwrites flush — the failed-fsync trap of §Checkpoint failure
  semantics, laundered through the bound.

Each meta persists `AnchoredDurableTxnID` — the anchored epoch as
known (completed) by its writer — so a peer acquiring the grant can
bound reclamation without a channel beyond the meta itself; the
live writer may additionally use its own newer in-process
anchoring knowledge (fsyncs it has observed complete).

**The tear-safe persist channel.** A SELF-DURABLE meta whose
persisted `AnchoredDurableTxnID` trails its own `DurableTxnID` —
the shape every fsyncing commit leaves behind (`SyncDurable` and
`SyncDataOnly` alike persist only the pre-commit anchored value,
per no-forward-promise) and every checkpointed self-durable meta
retains, because the checkpoint persist is deliberately withheld (§Checkpoint mechanics step 3: the meta is
the sole durable carrier of its own assertion, and an in-place
rewrite with CHANGED bytes risks a torn fsync destroying it) —
still lets a peer reach the full anchor: the adopting handle may
advance to the meta's own `DurableTxnID` only through ITS OWN
completed fsync, by rewriting the active slot BYTE-IDENTICALLY and
then fdatasync'ing. This mirrors the gated Open's recovery rewrite
and inherits both of its load-bearing properties: byte-identical
content makes any torn write harmless (every mix of identical
bytes is identical — the sole carrier cannot be destroyed, which
is exactly what a changed-bytes persist could not guarantee), and
the rewrite re-dirties the page so a previously-failed fsync's
consumed writeback error cannot let the gate's fdatasync succeed
trivially. A failed gate write or fsync leaves the anchor
unadvanced — conservative: reclamation stays delayed, the bound
never names an assertion the disk did not witness. Byte-identity
is VERIFIED, not assumed: before writing, the gate reads the slot
back and compares; any divergence (an encode/decode drift, foreign
nonzero padding on a checksum-valid meta) skips the gate the same
conservative way — a changed-bytes rewrite of the sole carrier is
exactly the hazard this channel exists to avoid. The gate runs
LAZILY on the eager reclamation path (background maintenance and
compaction), and only when the trailing anchor is the binding
constraint with pending retirements in the `[anchored, durable)`
window; allocation-pressure reclamation does not consult it — a
handle's own next completed fsync subsumes the advance anyway
(it anchors everything pwritten before it). The persisted FIELD
catches up at the handle's next commit, whose new meta lands in
the OTHER slot — tear-safe by the dual-slot protocol. (Pinned by
TestGateAnchorAdvanceUnblocksReclaim,
TestGateAnchorAdvanceFailureConservative,
TestGateAnchorAdvanceDivergenceSkips, and
TestGateAnchorAdvanceSkips; the withheld checkpoint persist by
TestCheckpointSelfDurableAnchorsInProcessOnly; the peer-bump
resync refresh by TestPeerCheckpointBumpSurvivesNextCommit and
TestPeerCheckpointBumpThenOwnCheckpointSkips.) After a
crash, anything read from disk is durable by definition, so a
freshly-recovered handle treats the selected meta's `DurableTxnID`
itself as anchored — and because a PROCESS crash leaves the OS page
cache intact (the read may not be a disk fact), the gated writable
Open makes this unconditional by fsyncing once itself: the recovery
commit's own fdatasync, or — when the selected meta is already
self-durable — a rewrite of that meta to its own slot followed by
fdatasync. The rewrite is load-bearing: a prior failed fsync both
consumes the kernel's writeback error and marks the pages clean, so a
bare fdatasync could succeed trivially, anchoring an assertion the
disk never received. Byte-identity of that rewrite is VERIFIED, not
assumed (the same rule the live-peer anchor gate enforces): the slot
is read back and compared against the re-encode, and a divergent
carrier — a checksum-valid meta whose nonzero padding a foreign or
older-format writer left (decode ignores it, encode zeroes it, the
checksum covers it) — is never rewritten; the open takes the
recovery-commit publication instead (the next TxnID to the other
slot), which is tear-safe by dual-slot and cannot create the
equal-TxnID pair that undefines meta selection. The reclamation bound is
`min(oldestActiveReaderTxnID, anchoredEpoch)` (`free-space.md §RPL
Reclamation`); recovery adoption is unaffected (it reads
`DurableTxnID` from disk, where anchoring is a tautology).

### Clean shutdown

`DB.Close()` on a writable handle performs the Checkpoint
sequence (steps 1–4: grant, data fdatasync, sub-record bump in
the active slot, meta fdatasync) before teardown, so a clean
close never loses acknowledged commits regardless of `SyncMode` —
a pure-`SyncLazy` application that never calls `Checkpoint()`
still reopens with everything it committed. Rollback to an older
durable epoch is reachable only through a real crash (or a
failed/killed Close). A Close issued while the handle's own live
write transaction still holds the grant SKIPS the shutdown
checkpoint (waiting would deadlock: the transaction cannot release
until after Close returns) — closing mid-transaction is not a
clean close; a warning is logged. An already-POISONED handle SKIPS
the shutdown checkpoint: running it would be exactly the retried-fsync
trap of §Checkpoint failure semantics (the retry succeeds
trivially over kernel-consumed error state and stamps a durable
sub-record on data that never reached storage). The poisoned state
was already surfaced as `ErrPoisoned` by the operation that
poisoned; Close itself tears down normally and returns nil —
re-Open converges. A handle whose data file a peer's `Compact` has
replaced (generation mismatch) also SKIPS: its mapped inode is
unlinked and invisible, and every acknowledged commit was
serialized — under the grant — into the peer's fsynced compacted
file; there is nothing on the old inode worth persisting. A
checkpoint failure DURING Close follows Checkpoint's failure
semantics except that poison is moot — the handle is closing; the
failure is surfaced as Close's error. Read-only handles skip this
step. (Pinned by `TestCleanCloseCheckpointsSyncLazy`,
`TestPoisonedCloseSkipsShutdownCheckpoint`, and
`TestCloseWithLiveWriteTxSkipsShutdownCheckpoint`.)

### `Checkpoint()` mechanics

1. Acquire the write lock via the flock goroutine — same path as
   `Begin(writable=true)`, respecting the supplied `ctx`. This
   serialises Checkpoint against any concurrent write transaction
   and any concurrent `Compact()` in the queue; concurrent reads
   are unaffected. Returns `context.Cause(ctx)` if cancelled
   before the lock is acquired.
2. `fdatasync(fd)` to flush all data, RPL, bitmap, and meta
   pages pwritten by prior `SyncLazy` commits that are sitting
   in the OS page cache. (The data mmap is `PROT_READ` and the
   writer never writes through it, so there are no mmap dirty
   pages from gmdb; the fdatasync's job is purely to flush
   pwritten page-cache contents.)
3. Read the currently active meta page; set its durable
   sub-record to the meta's own live state (`DurableTxnID =
   TxnID`, durable fields copied from the live fields), and set
   `AnchoredDurableTxnID` to the PRE-bump anchored value (step
   2's completed fsync anchors the pre-bump meta's assertion;
   the bump's own assertion is anchored by step 4 and persisted
   by the NEXT meta write — §Anchoring's no-forward-promise
   rule); recompute the XXH3-64 checksum over the full meta
   payload; `pwrite()` it back to the same slot. The TxnID is
   unchanged — Checkpoint records that the already-committed
   state is durable, not a new transaction. A meta that is
   ALREADY self-durable skips the bump-and-pwrite; steps 2 and 4
   still run (step 4 lands the previously-written sub-record on
   stable storage even when the prior commit was `SyncDataOnly`,
   which skipped its own step 4). The skip is LOAD-BEARING, not
   an idempotence elision: a self-durable meta is the SOLE
   durable carrier of its own assertion (the other slot's
   sub-record predates it), and rewriting it in place — even
   only to persist the step-2 anchor advance — risks a torn
   step-4 fsync (the kernel consumes the writeback error and
   marks the page clean) destroying the assertion on disk while
   the intact page-cache copy keeps feeding peer reclamation
   bounds: after a crash, recovery falls back to the other,
   OLDER slot, whose tree references pages the bound let a peer
   reuse — page aliasing. A non-self-durable bump has no such
   hazard (its sub-record is carried in BOTH slots). Consequence
   of the skip: the persisted `AnchoredDurableTxnID` deliberately
   TRAILS the in-process anchored epoch in pure `SyncDataOnly`
   use — delayed peer reclamation, never unsafety; a peer closes
   the gap through the tear-safe persist channel (§Anchoring),
   never through a changed-bytes rewrite of this carrier.
4. `fdatasync(fd)` again so the sub-record bump itself reaches
   stable storage.
5. Release the write lock.

Steps 2 and 4 are both required: step 2 makes prior lazy commits
durable; step 4 makes the sub-record bump durable so recovery can
trust it. The single-meta-slot pwrite in step 3 is atomic because
it stays within one page (an unaligned tear cannot affect a single
contiguous sub-page region, and the XXH3-64 checksum catches
any partial write — recovery falls back to the other slot).

Bounded live-read anomaly: a LOCK-FREE cross-process reader (a
read-only handle on a database with no lock file access) can
read the active meta slot concurrently with step 3's pwrite and
observe a torn/invalid checksum for that instant. This is
harmless by construction: the reader's meta selection rejects
the invalid slot on checksum and falls back to the OTHER slot —
a valid, older meta — and reclamation never runs under a
sub-record bump (the TxnID is unchanged, so the snapshot
the fallback pins is reclamation-protected exactly like any
other reader of that meta). The anomaly is a transient
one-instant stale read, never a wrong or unprotected snapshot.
In-process readers and lock-coordinated peers are unaffected
(their meta reads go through the handle's published
currentMeta / the restabilization loop).

### Checkpoint failure semantics

Steps 2–4 of the checkpoint sequence (data fdatasync, active-slot
meta pwrite, meta fdatasync) are Checkpoint's publication phase. Any
failure there POISONS the handle — every subsequent
transaction-opening operation returns `ErrPoisoned`, and Close +
re-Open is the only recovery — mirroring the commit pipeline's
publication contract:

- A failed fdatasync consumes the kernel's per-fd error state while
  marking the pages clean; a retried Checkpoint's fdatasync then
  succeeds trivially and stamps a durable sub-record over data that
  never reached stable storage — recovery adopts the sub-record
  and traverses into unwritten pages (the exact violation
  §Durability Modes warns about).
- A torn active-slot pwrite leaves the only on-disk copy of the
  active meta checksum-invalid while the process keeps serving it
  from memory; a peer writer's re-sync then selects the other
  (older) slot and commits its own tree over pages the newer tree
  references — split brain, page aliasing.

For the torn-write case, poison BOUNDS the divergence rather than
preventing it — the torn slot is already on disk and the write grant
passes to peers regardless; poison stops this handle from continuing
to serve and extend a tree the disk no longer describes, and re-Open
converges it to the peers' view.

Failures BEFORE step 2 (grant acquisition, re-sync) leave disk and
pager state untouched and do not poison. Re-Open re-reads the actual
on-disk state, so a poisoned handle converges instead of compounding.
(Enforced in `DB.Checkpoint`; pinned per step by
`TestCheckpointPublicationFailurePoisonsHandle`, with the post-grant
re-check and the no-poison clause pinned by
`TestCheckpointPoisonEdges`.)

### Directory-entry durability

fdatasync on the database file makes its BYTES durable; POSIX makes
the directory ENTRY durable only after the parent directory is
fsynced. Three sites carry that obligation:

- **Every writable Open.** Open fsyncs the parent directory on every
  writable open, not only the creating one: a create-retry after a failed dir fsync and an Open
  racing a creator that crashed before its fsync both land on the
  existing-file path, so a creation-only sync would leave those
  dirents non-durable forever. Failure fails the Open. Without the
  obligation, power loss after N acked SyncDurable commits can leave
  the file absent — total loss of a "durable" database. Read-only
  opens skip it (nothing to make durable; read-only media would
  reject the fsync). The lock file is exempt: transient coordination
  state Open recreates.
- **CopyTo.** The copy's bytes are fsynced before return; the output
  file's dirent is made durable by fsyncing its parent directory,
  else the "completed" backup can vanish in a crash.
- **Compact's rename.** The new inode replaces the old atomically,
  but the replacement is durable only after the directory fsync;
  failure poisons the handle (the on-disk outcome is unknowable — a
  crash may resurrect the old inode while this handle would serve
  the new one).

(Enforced by `syncDir`/`syncDirPath` at all three sites; pinned by
`TestOpenSyncsParentDir` and
`TestCompactDirSyncFailurePoisons`.)


## Recovery

On recovery (Open after crash):

1. Read both meta pages. Discard any with invalid XXH3-64
   checksum.
2. Of the valid metas, select the one with the highest `TxnID` —
   the SAME selection every live path uses (`file-layout.md
   §Meta Page`, active-meta paragraph and the equal-TxnID
   invariant).
3. Adopt the selected meta's **durable sub-record** as the
   recovered state: keyspace root and count, HighWaterMark, RPL
   pointers, and free-page count all come from the sub-record,
   never from the selected meta's live fields. When the meta is
   self-durable (`DurableTxnID == TxnID` — the last commit
   fsynced, or `Checkpoint()` ran), the two projections
   coincide and nothing is lost.
4. Neither meta valid → the database is corrupt (`ErrCorrupted`).
   A writable Open defers building its in-memory state until step
   5's gate decides WHICH projection to attach: attaching the live
   projection first would walk a possibly-unflushed post-epoch RPL
   head — exempt from boundary treatment, hence a hard error — and
   permanently fail an Open whose durable projection is intact.
5. **Recovery commit** — writable Open only, when the open
   establishes the database has **no live author** AND either
   `DurableTxnID < TxnID`, or the selected meta is self-durable
   but its on-disk carrier diverges byte-wise from the re-encode
   (§Anchoring's verified-identity rule — a foreign writer's
   checksum-valid nonzero padding; the anchor rewrite must not
   change the carrier's bytes and an equal-TxnID copy would
   undefine selection, so the divergent case republishes here).
   The no-live-author gate: the lock file was freshly created, or its persisted
   last-writer record and every reader slot classify as dead/stale
   (`cross-process.md §Lock File Layout`, LastWriter*; §Reader
   Table). The last-writer record — written at grant acquisition,
   surviving grant release, heartbeated for the author handle's
   lifetime — is the load-bearing signal: only the last writer's
   process can own unfsynced live commits, and it may be IDLE
   (holding no grant and no reader slots) while still serving
   them. A live author means the selected meta's live tree is a
   running database's current state — a joining writer must NOT
   roll it back. The selection the recovery commit adopts and
   publishes is (re)established UNDER the same grant — a pre-grant
   snapshot can be stale by any number of peer commits that landed
   while the grant acquisition blocked, and publishing from it
   would overwrite an acknowledged peer commit and retreat the
   durable epoch. When the gate passes: under
   the write grant, publish the adopted state as a fresh meta at
   `TxnID + 1` (live fields = the adopted durable state;
   `DurableTxnID = TxnID + 1`, which is data-safe unfsynced — its
   tree IS the durable epoch's; `AnchoredDurableTxnID` = the
   adopted epoch, the disk-proven value, per §Anchoring's
   no-forward-promise rule) to the non-selected slot, then
   `fdatasync` (after which the handle's in-process anchored
   epoch is `TxnID + 1`). This out-selects the rejected live tree
   for every subsequent selection and is idempotent under crash
   (a crash before the fsync leaves the old slots authoritative;
   recovery re-runs). Read-only opens cannot repair; see the
   window note below.

**The unrecovered window.** Between a crash and the first writable
Open's recovery commit, the on-disk active slot still names the
rejected live tree. A read-only handle attached in that window
selects it and uses the live projection (`cross-process.md §Reader
Table`) — its traversals can reach partially-flushed pages. This is
bounded and detectable, not silent: page checksums (on by default)
surface such pages as `ErrBadPageChecksum`. The on-disk condition
ends at the first gated writable Open, but a read-only handle that
attached DURING the window keeps its selected live projection for
that handle's lifetime — and the rejected tree's pages are free in
the adopted bitmap, so post-recovery writers may overwrite them
under such a reader (checksum-detectable, never silent). Read-only
access to a crashed, never-recovered database is best-effort by
nature; a deployment needing clean read-only access after a crash
opens one writable handle first (and re-opens read-only handles
that predate it).


Commits in `(DurableTxnID, TxnID]` — `SyncLazy` commits after the
last fsync point — are lost by design: that is the `SyncLazy`
trade. Recovery never adopts a tree that is not guaranteed
durable; the sub-record's tree is intact because CoW never
modifies existing pages and RPL reclamation never frees pages the
durable epoch's tree references (`free-space.md §RPL
Reclamation`). A database that was never made durable past
initialization recovers to its (fsynced) genesis state — the
honest expression of "nothing was ever made durable".

The selected meta's live fields still matter to recovery for
policy: the file-format fields (`MinSize`, `GrowStep`,
`ShrinkThreshold`) are taken live per §Checkpoints and the
durable sub-record. A `SetFileFormat` change committed after the
durable epoch therefore SURVIVES recovery iff its meta occupies
the selected slot — nondeterministic across the two-slot
rotation, and safe either way (policy pairs with any tree;
`MaxSize` is immutable and shrink floors at the adopted
HighWaterMark).

## Cross-process SyncMode interleaving

`SyncMode` is a per-process `Options` setting, not stored on
disk. Different processes attached to the same database may run
with different SyncModes. The durable sub-record reflects
whichever fsync-ing event happened last, regardless of process: a
commit by a `SyncDurable` process writes a self-durable meta; a
commit by a `SyncLazy` process carries the previous sub-record
forward. Recovery adopts the highest-`TxnID` valid meta's
sub-record, so interleaving `SyncLazy` and `SyncDurable` writers
across processes works correctly — a crash rolls back to the
most recent fsync point, possibly losing intervening `SyncLazy`
commits from any process. This is the same trade-off as
`SyncLazy` within a single process; the multi-process composition
is consistent with that.

**One selection, two projections.** Live operation and crash
recovery select the SAME meta — the highest-`TxnID` valid slot.
They differ only in which projection of it they use: a writer
re-syncing on a grant handoff and a reader beginning a
transaction use the **live projection** (the meta's own tree —
`cross-process.md §Writer acquisition flow` / §Reader Table), so
an unfsynced `SyncLazy` commit IS visible to other live handles
and is built upon by the next writer; crash recovery uses the
**durable projection** (the sub-record). A `SyncLazy` commit is
therefore *visible-while-live but not crash-durable*, expressed
by one selection rule rather than two. The grant serializes
writers, so a live handoff is never a torn read; the durable
projection is invoked by recovery (and by nothing else), and the
recovery commit (§Recovery step 5) republishes it as the live
state, so live operation never resumes atop a tree recovery
rejected — modulo the unrecovered read-only window noted in
§Recovery.

package gmdb

import (
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"sort"
	"strings"
	"sync/atomic"
	"time"
	"unique"

	"github.com/greatliontech/gmdb/internal/btree"
	"github.com/greatliontech/gmdb/internal/closegate"
	"github.com/greatliontech/gmdb/internal/descriptor"
	"github.com/greatliontech/gmdb/internal/lock"
	"github.com/greatliontech/gmdb/internal/pager"
)

// flushFailHookForTest fires at the top of flushKeyspaces — the seam
// for driving the commit's assembly-phase (pre-pipeline) failure path,
// which the flush reserve makes unreachable through ordinary fixtures.
var flushFailHookForTest atomic.Pointer[func() error]

// Tx is a write transaction. Its surface is keyspace and index
// management (Open / Create / Delete keyspaces, Rebuild / Drop
// indexes), nested transactions (BeginChild), and lifecycle (Commit,
// Rollback). Data operations go through the *Keyspace / *SetKeyspace
// handles it returns and their cursors and typed wrappers.
//
// Byte slices read through a write transaction (own-writes via a
// Keyspace handle) are valid until Commit / Rollback completes; do not
// retain them past tx close (api-surface.md §Byte Slice Ownership).
type Tx struct {
	db *DB

	// pgr is captured at Begin so Tx methods don't race a concurrent
	// db.Close (which nil's db.pgr under db.mu). Once Begin returns
	// a *Tx, pgr is stable for the tx's lifetime; the pager's heap
	// state survives independently of db.pgr nil'ing. Use-after-Close
	// is gated by close-gate checks at method entry rather than by
	// re-reading db.pgr.
	pgr *pager.Pager

	prevMeta   pager.Meta
	prevActive int
	newTxnID   uint64
	writable   bool
	closed     bool

	// startTime / endTime bound the transaction's lifetime for
	// TxStats.Duration. startTime is stamped at Begin; endTime is stamped
	// when the tx is finalized (Commit / Rollback). While the tx is live
	// (endTime zero) Stats reports time.Since(startTime).
	startTime time.Time
	endTime   time.Time

	// pendingFileFormat holds a Tx.SetFileFormat override of the mutable
	// file-format meta fields (MinSize/GrowStep/ShrinkThreshold, in pages),
	// applied to the new meta at Commit. nil ⇒ no change this tx. The change
	// is persisted atomically with the commit and takes effect from the NEXT
	// transaction (the committed meta carries it; the next Begin's
	// BeginTx(TxParams) reloads the pager from it).
	pendingFileFormat *pager.MetaFileFormat

	// keyspaceRoot and numKeyspaces track the in-progress state of
	// the keyspace B+tree (see keyspaces.md + file-layout.md §Meta
	// Page). Seeded from prevMeta at Begin; CreateKeyspace /
	// DeleteKeyspace mutate them via btree.Put/Delete on the keyspace
	// B+tree (with the pager as PageWriter); Commit passes the updated
	// values to pager.Commit so they land in the new meta. A
	// transaction that does not touch the keyspace surface leaves
	// these at their prevMeta values.
	keyspaceRoot uint64
	numKeyspaces uint64

	// openKeyspaces caches *Keyspace handles by interned name within
	// this transaction. unique.Make[string] guarantees that repeated
	// OpenKeyspace calls for the same name produce the same
	// unique.Handle[string], so cache lookup is O(1) pointer compare
	// (keyspaces.md §Keyspace Name Interning). Populated on
	// successful Open / Create; DeleteKeyspace removes the entry
	// (and migrates the *Keyspace to deadKeyspaces with dead=true so
	// post-Delete handle ops return ErrKeyspaceClosed per the
	// api-surface.md §Keyspace API DeleteKeyspace permanent-invalidation invariant).
	openKeyspaces map[uniqueNameHandle]*Keyspace

	// openSetKeyspaces caches *SetKeyspace handles by interned name
	// within this transaction — kind-symmetric partner of
	// openKeyspaces for Kind=1 keyspaces. Same lifecycle: populated
	// on successful OpenSetKeyspace / CreateSetKeyspace*;
	// DeleteKeyspace removes the entry (and migrates the *SetKeyspace
	// to deadKeyspaces with dead=true). A keyspace name is in at
	// most one of {openKeyspaces, openSetKeyspaces} at any time
	// (Kind-immutability + the Open Kind-mismatch check together
	// ensure a name resolves to exactly one Kind per tx).
	openSetKeyspaces map[uniqueNameHandle]*SetKeyspace

	// dirtyDescriptors holds descriptor mutations on names that have
	// no handle in openKeyspaces / openSetKeyspaces. Writers:
	// SetKeyspaceConfig on an unopened name (kind-agnostic per the
	// godoc on api-surface.md §Keyspace API SetKeyspaceConfig, so the
	// mutation cannot be carried on a *Keyspace which is Kind=0
	// only); the index-admin adapter path (TxIndexes.Drop / Rebuild
	// on an uncached keyspace via propagateNotCachedDescChange); and
	// compactForest's relocated-root staging. These are flushed at
	// Commit (the deferred-flush refactor) alongside openKeyspaces
	// with state ∈ {created, dirty}. A later same-tx open of a staged
	// name MOVES the entry's flush obligation into the cached handle
	// (openCacheState seeds state=dirty, then the open deletes the
	// entry); DeleteKeyspace supersedes an entry into pendingDeletes.
	// Invariant: a name is in at most one of {openKeyspaces,
	// openSetKeyspaces, dirtyDescriptors, pendingDeletes}.
	dirtyDescriptors map[string]descriptor.Keyspace

	// pendingDeletes is the set of keyspace names deleted in this tx
	// whose descriptor existed on disk pre-tx. DeleteKeyspace removes
	// the descriptor row from the keyspace B+tree EAGERLY (the tx's
	// CoW view), so this map carries no commit-time work — it survives
	// as the same-tx semantic overlay consulted by opens / creates /
	// lookups. A Created-this-tx-then-Deleted name is NOT added here;
	// a Delete-then-Create of the same name removes the entry.
	pendingDeletes map[string]struct{}

	// ksPathLen caches the keyspace B+tree's root-to-leaf page count
	// for the commit-flush reserve (recalcFlushReserve): each flush
	// write is a same-size descriptor upsert costing exactly one CoW
	// page per path level. Measured lazily on the first flush
	// obligation (ensureKeyspacePathLen) and refreshed by the eager
	// keyspace DDL (create insert / delete removal), the only
	// operations that change the tree's structure — same-size updates
	// cannot split or merge. 0 = not yet measured.
	ksPathLen int

	// deadKeyspaces holds every *Keyspace handle invalidated by a
	// same-tx DeleteKeyspace, including those whose name has since
	// been re-created (spec: api-surface.md §Keyspace API
	// DeleteKeyspace — re-creating the keyspace in the same tx does
	// NOT reactivate the old handle). Each holds dead=true; their
	// methods return ErrKeyspaceClosed. The slice keeps the *Keyspace
	// reachable from the tx so future ops on the handle find their
	// way back to the tx's closed-state checks; on Commit/Rollback
	// the tx is dropped and the dead handles become unreachable along
	// with it.
	deadKeyspaces []*Keyspace

	// deadSetKeyspaces is the Kind=1 partner of deadKeyspaces:
	// every *SetKeyspace handle invalidated by a same-tx
	// DeleteKeyspace. Lifecycle and reachability semantics are
	// identical (see deadKeyspaces godoc). A re-created keyspace in
	// the same tx returns a fresh handle; the old stays dead.
	deadSetKeyspaces []*SetKeyspace

	// held tracks whether this Tx still owns the cross-process write
	// grant. Begin sets it to true; Commit, Rollback, and the
	// runtime.AddCleanup callback each attempt a single
	// CompareAndSwap(true, false) — the winner is responsible for
	// grant.Release + (in the cleanup case) the leak-warning log +
	// AbortTx. lock.Grant.Release is already sync.Once-guarded, so
	// double-release is structurally impossible; held coordinates the
	// "did the user already close?" decision for leak warnings.
	// Stored as a pointer so the cleanup info struct can hold it
	// without referencing the *Tx (runtime.AddCleanup forbids cleanup
	// args from referencing the cleaned-up object — resurrection is
	// not permitted).
	held *atomic.Bool

	// grant is the cross-process write-lock grant from
	// db.coord.AcquireWriter; nil for read transactions. Released via
	// grant.Release() on Commit / Rollback / GC cleanup.
	grant *lock.Grant

	// cleanup is the AddCleanup handle, Stop()'d by Commit/Rollback in
	// the normal close path so the leak-detection warning doesn't fire
	// for a tx the caller properly closed.
	cleanup runtime.Cleanup

	// parent is the enclosing transaction when this Tx is a child
	// created via BeginChild; nil for a top-level write transaction. A
	// child shares the parent's *Pager and (transitively) the top-level
	// parent's cross-process write grant — only the top-level parent
	// holds the lock and commits to disk (transactions.md §Nested
	// Transactions). A child's prevMeta / newTxnID / pgr are copied from
	// the parent; held and grant are nil (the child releases nothing).
	parent *Tx

	// activeChild is this tx's currently-open, unresolved child (from
	// BeginChild), or nil. While non-nil this tx — and transitively
	// every ancestor — is FROZEN: data ops, Commit, and a second
	// BeginChild return ErrChildActive until the child commits or
	// rolls back (parent-freeze / LMDB nested-txn model,
	// transactions.md §Nested Transactions); Rollback instead
	// cascades through the chain. Cleared by the child's
	// commitChild / rollbackChild / cascadeRollback.
	activeChild *Tx

	// savepoint is the pager savepoint captured at BeginChild; nil for a
	// top-level tx. The child's Commit releases it (page-level mutations
	// merge into the shared pager, published at the top-level Commit);
	// the child's Rollback restores it (mutations discarded). See
	// internal/pager/savepoint.go.
	savepoint *pager.Savepoint
}

// txCleanupInfo is the argument bundle for the AddCleanup callback.
// Deliberately omits the *Tx: runtime.AddCleanup rejects an arg that
// reaches the obj, since resurrecting the obj would defeat collection.
//
// Captures the shared *closegate.Gate by pointer (leak-detection.md
// clause-explicit invariant — required because runtime.AddCleanup
// provides no ordering between the DB cleanup and Tx cleanups, and
// the gate was promoted from a plain *atomic.Bool to a
// *closegate.Gate with an additional inflight-cleanup refcount so
// Close can drain in-flight cleanups before unmap). Also captures
// *Pager and *Grant directly (not via *DB) so a concurrent
// DB.Close — which sets db.pgr = nil and db.coord = nil — does not
// nil-deref this callback.
type txCleanupInfo struct {
	gate      *closegate.Gate
	pgr       *pager.Pager
	grant     *lock.Grant
	held      *atomic.Bool
	logger    *slog.Logger
	originPCs []uintptr
}

// txCleanupFn is the leak-detection callback invoked by
// runtime.AddCleanup some time after the *Tx becomes unreachable. The
// CompareAndSwap on info.held ensures a single releaser —
// Commit/Rollback's call to releaseGrant contests the same atomic, so
// the cleanup is a no-op for transactions the caller closed normally.
//
// Spec contract (leak-detection.md §Cleanup Behavior clause-explicit
// invariants): observing the gate closed MUST return without
// touching the reader-table mmap or signalling the flock goroutine.
// We DO log the leak warning either way — the warning is the user-
// facing signal that they forgot to Commit/Rollback; suppressing it
// during Close would lose the diagnostic.
//
// Non-blocking constraint: this callback runs on the GC background
// goroutine. atomic ops + grant.Release (sync.Once → channel close)
// + slog handler diagnostic write are all admitted; no mutex
// acquisition, no blocking syscall, no panic.
func txCleanupFn(info txCleanupInfo) {
	if !info.held.CompareAndSwap(true, false) {
		return
	}
	info.logger.Warn(
		"gmdb: write transaction leaked without Commit/Rollback",
		"origin", formatStack(info.originPCs),
	)
	if !info.gate.EnterCleanup() {
		// DB closed — its Close path drained the Coord goroutines
		// and Released any held grants. Touching info.pgr (whose
		// internal mmap is unmapped) would risk SIGSEGV on a future
		// pager change that touches mmap from AbortTx; skip per
		// spec invariant. The Coord-bound grant channel's other
		// side is gone, so grant.Release would be a no-op anyway.
		// gate.ExitCleanup still required so Close's drain doesn't
		// see a phantom inflight counter.
		info.gate.ExitCleanup()
		return
	}
	defer info.gate.ExitCleanup()
	// Restore in-memory pager state to pre-tx, then release the
	// cross-process write lock. AbortTx must precede grant.Release —
	// once the grant is released the flock goroutine clears the
	// writer header and unlocks; another process or this process's
	// next Begin observes the now-released lock and runs BeginTx,
	// snapshotting state that would otherwise include allocations
	// the leaked tx made but never committed.
	info.pgr.AbortTx()
	info.grant.Release()
	if hook := txCleanupHookForTest.Load(); hook != nil {
		(*hook)()
	}
}

// txCleanupHookForTest, when set, is invoked at the tail of
// txCleanupFn's active-release path (after info.grant.Release),
// INSIDE the EnterCleanup/ExitCleanup window. Same contract as
// readTxCleanupHookForTest: the installed callback MUST be
// non-blocking per leak-detection.md §Cleanup Behavior. It exists
// because the branch's effects are otherwise unobservable —
// grant.Release is sync.Once-idempotent, so a test cannot
// distinguish "released during cleanup" from "released by a later
// Rollback" without a signal from inside the branch.
var txCleanupHookForTest atomic.Pointer[func()]

// setTxCleanupHookForTest installs (or clears, when hook==nil) the
// test-only post-release synchronization hook described on
// txCleanupHookForTest. Test-only.
func setTxCleanupHookForTest(hook func()) {
	if hook == nil {
		txCleanupHookForTest.Store(nil)
		return
	}
	txCleanupHookForTest.Store(&hook)
}

// captureOriginPCs records the call stack at Begin so the cleanup
// warning can point at where the leaked transaction was opened.
// Returns raw PCs; formatting is deferred to log time via formatStack.
//
// skip=3 drops three frames from the trace: runtime.Callers itself
// (per its docs, skip=0 is its own frame), captureOriginPCs, and
// (*DB).Begin. The first recorded frame is therefore the user's
// caller — what the warning actually wants to point at.
func captureOriginPCs() []uintptr {
	pcs := make([]uintptr, 32)
	n := runtime.Callers(3, pcs)
	return pcs[:n]
}

// formatStack renders PCs from captureOriginPCs into a human-readable
// multi-line trace, lazy so a never-fired cleanup pays nothing.
func formatStack(pcs []uintptr) string {
	if len(pcs) == 0 {
		return "(stack unavailable)"
	}
	var b strings.Builder
	frames := runtime.CallersFrames(pcs)
	for {
		f, more := frames.Next()
		fmt.Fprintf(&b, "\n  %s\n    %s:%d", f.Function, f.File, f.Line)
		if !more {
			break
		}
	}
	return b.String()
}

// Commit publishes the transaction's changes via the four-step commit
// protocol (pager-slab.md §Commit Write Ordering). On success the DB's
// active meta advances and the write-lock is released.
//
// Descriptor flush (deferred-flush refactor). All same-tx
// descriptor mutations (Keyspace.Put / Delete / Cursor.Delete /
// Tx.SetKeyspaceConfig / Tx.CreateKeyspace* / Tx.DeleteKeyspace) are
// kept in memory on the *Keyspace handles, in tx.dirtyDescriptors,
// and in tx.pendingDeletes. Commit walks these sets and applies the
// final state to the keyspace B+tree before pager.Commit's four-step
// protocol begins. A flush failure cleans up via the normal AbortTx
// path (bitmap snapshot restore, slab pool return, retired+loose
// page clearance) and the on-disk state is unchanged — no new poison
// machinery is needed because no on-disk write has yet been issued.
// The motivating case (a partial-mutation failure mode where a
// per-op storeDescriptor write could fail AFTER a successful data-tree
// mutation, producing on-disk orphan pages on a subsequent Commit) is
// closed by this design move: the two-write-no-atomicity window per
// per-op storeDescriptor disappears in favour of a single commit-time
// apply.
func (tx *Tx) Commit() error {
	if err := tx.requireOpen(true); err != nil {
		return err
	}
	// A child transaction commits by merging into its parent (no disk
	// write, no grant release) — see commitChild.
	if tx.parent != nil {
		return tx.commitChild()
	}
	// Cancel the leak-detection cleanup first — it must not fire if the
	// caller is closing the tx explicitly. Safe to Stop even if the
	// cleanup has already executed (Stop is idempotent per
	// runtime.AddCleanup's contract).
	tx.cleanup.Stop()
	defer tx.releaseGrant()
	tx.closed = true
	tx.endTime = time.Now() // TxStats.Duration: tx open Begin → Commit
	tx.pgr.SetCurrentTxnID(tx.newTxnID)
	// Enter the pager's commit phase before the descriptor flush: the
	// flush's allocations are exactly what the external commit
	// reserve was maintained for (recalcFlushReserve), so they draw
	// from the reserved space. pager.Commit manages the flag for its
	// own steps; AbortTx/BeginTx reset it on the failure paths.
	// Pre-commit spill (pager-slab.md §Slab Budget, spill threshold):
	// bring the slab under the ops-phase limit before the commit
	// phase, so the descriptor flush and step-0 RPL assembly have
	// their reserved headroom regardless of how large the transaction
	// grew. All savepoints are resolved here, so every non-loose
	// dirty page is eligible.
	tx.pgr.SpillExcess()
	tx.pgr.SetCommitPhase(true)
	// Capture the touched-keyspace notification slots BEFORE the
	// descriptor flush marks the handles clean; published only after
	// the commit's meta publication (cross-process.md §Lock File
	// Layout, notification region: stamp happens-after visibility).
	notifySlots := tx.touchedNotifySlots()
	if err := tx.flushKeyspaces(); err != nil {
		// Flush failed before pager.Commit ran — AbortTx is sufficient.
		// No on-disk pwrite has happened yet (pager.Commit's step-1
		// runs later), so no DB-wide poisoning; trivially not-visible
		// (durability.md §Commit Outcome Classification). The caller
		// can retry in a fresh tx. A failed best-effort spill can be
		// the real cause of a budget failure here (the slab stayed
		// over-threshold) — name it.
		if serr := tx.pgr.SpillError(); serr != nil && errors.Is(err, pager.ErrTxTooLarge) {
			err = fmt.Errorf("%w (after a degraded spill: %w)", err, serr)
		}
		tx.pgr.SetCommitPhase(false)
		tx.pgr.AbortTx()
		return fmt.Errorf("%w: %w", ErrCommitNotVisible, err)
	}
	// SyncMode → pager SyncPolicy. Flags carry only the immutable
	// PageChecksum bit; the durable sub-record — the recovery contract
	// the retired checkpoint flag used to approximate — is composed by
	// the pager from the policy (durability.md §Checkpoints and the
	// durable sub-record).
	flags := tx.prevMeta.Flags & pager.MetaFlagPageChecksum
	syncPolicy := pager.SyncBoth
	switch tx.db.opts.SyncMode {
	case SyncDurable:
		syncPolicy = pager.SyncBoth
	case SyncDataOnly:
		syncPolicy = pager.SyncDataOnly
	case SyncLazy:
		syncPolicy = pager.SyncNone
	}
	result, err := tx.pgr.Commit(pager.CommitParams{
		NewTxnID:      tx.newTxnID,
		KeyspaceRoot:  tx.keyspaceRoot,
		NumKeyspaces:  tx.numKeyspaces,
		Flags:         flags,
		Sync:          syncPolicy,
		SetFileFormat: tx.pendingFileFormat,
	}, tx.prevMeta, tx.prevActive)
	if err != nil {
		// A commit can fail in the no-syscall ASSEMBLY phase (step 0:
		// tail-refund, loose migration, RPL segment slab allocation /
		// file extension) or in the PUBLICATION phase (step 1+: data /
		// bitmap / RPL pwrites, fsync, meta pwrite). pager.Commit runs
		// AbortTx on either, restoring the in-memory bitmap / HighWaterMark
		// / RPL chain to the pre-tx snapshot.
		//
		// Publication-phase failures leave in-memory disagreeing with disk
		// (step-1 pwrites already hit the file; step-3 may have published a
		// new meta) — the handle cannot safely continue and must be poisoned;
		// recovery is Close + re-Open, exactly as after a writer crash.
		//
		// Assembly-phase failures are fully reversible: nothing was pwritten,
		// and AbortTx restored consistency, so the handle stays usable (the
		// caller may retry in a fresh, smaller tx). The only errors that can
		// originate in step 0 are ErrTxTooLarge (RPL-segment slab exceeds
		// MaxTxBufferBytes) and ErrDBFull (file extension hits MaxSize) —
		// step 1+ performs no allocation. Poisoning those would brick a
		// recoverable handle (e.g. a single large delete whose RPL append
		// overruns the budget, or background compaction's budget-halving retry (background-maintenance.md §Invariants)).
		// Outcome classification (durability.md §Commit Outcome
		// Classification): every Commit error wraps exactly one class
		// sentinel. Assembly failures are trivially not-visible (no
		// write was issued). Publication failures classify by META
		// READBACK under the STILL-HELD grant (releaseGrant is
		// deferred): if the latest valid on-disk meta is THIS tx's,
		// the commit is visible despite the error — a retry would
		// double-apply — and a step-4 fsync failure additionally
		// leaves stable-storage durability unknown.
		class := ErrCommitNotVisible
		classified := true
		if !errors.Is(err, pager.ErrTxTooLarge) && !errors.Is(err, pager.ErrDBFull) {
			tx.db.poisoned.Store(true)
			// The publication-phase pwrites that DID land are torn
			// unpublished state — bitmap bits for reclaimed segments,
			// with no meta publish — exactly the image a
			// died-holding-grant crash leaves, but the header will be
			// cleared by our clean release. Bump the takeover sequence
			// under the grant this tx still holds (deferred release)
			// so every surviving handle's next grant re-sync forces
			// the bitmap+RPL rebuild (free-space.md §Grant-handoff
			// tear detection). This handle itself is poisoned and
			// never writes again.
			if tx.db.coord != nil {
				tx.db.coord.BumpTakeoverSeq()
			}
			m, rerr := tx.pgr.ReadBackLatestMeta()
			switch {
			case rerr != nil:
				// The verification read itself failed (exotic — both
				// slots are page-cache preads): certainty is
				// unavailable, so NO class is attached rather than a
				// false one. NotVisible would invite a double-applying
				// retry; the class sentinels are certainty statements
				// (durability.md §Commit outcome classification,
				// unclassified failures). Callers must treat this as
				// do-not-retry and probe after re-Open.
				classified = false
			case m.TxnID == tx.newTxnID:
				// Published. Under SyncDurable the durability contract
				// includes step 4's meta fsync, which either failed or
				// never ran once anything after step 3 errored — the
				// commit is visible but its promised durability is
				// unestablished. The lazier modes never promised that
				// fsync, so a published commit there is exactly what
				// their contract delivers: Visible.
				if syncPolicy == pager.SyncBoth {
					class = ErrCommitDurabilityUnknown
				} else {
					class = ErrCommitVisible
				}
				// The commit IS visible to peers (verified readback);
				// waiters must not sleep through it. Publish under the
				// still-held grant, same as the success path.
				tx.publishCommitNotify(notifySlots)
			}
		}
		if !classified {
			return fmt.Errorf("gmdb: commit failed and the outcome could not be verified (do not retry; re-Open and probe): %w", mapPagerErr(err))
		}
		// Same degraded-spill context join as the flush path: a step-0
		// budget failure after a failed spill names its real cause.
		if serr := tx.pgr.SpillError(); serr != nil && errors.Is(err, pager.ErrTxTooLarge) {
			return fmt.Errorf("%w: %w (after a degraded spill: %w)", class, mapPagerErr(err), serr)
		}
		return fmt.Errorf("%w: %w", class, mapPagerErr(err))
	}
	tx.db.mu.Lock()
	// The reclamation bound no longer rides the meta adoption: the
	// pager advanced its anchored epoch at the commit's own fsyncs
	// (durability.md §Anchoring), and the next Begin derives the bound
	// from it.
	tx.db.setMetaState(result.Meta, result.ActiveMetaIdx)
	tx.db.mu.Unlock()
	// Notification publish (cross-process.md §Lock File Layout,
	// notification region): after the meta publication above (a woken
	// waiter's next read must observe this commit), under the grant
	// this tx still holds (releaseGrant is deferred) — grant
	// serialization is what makes the plain version stamps monotonic.
	tx.publishCommitNotify(notifySlots)
	// The pager's commit-state seeding (HighWaterMark, MaxSize,
	// reclamationBound) moved to the next write-tx Begin path
	// so the reclamation bound reflects the reader-table
	// scan AT begin-time rather than at the previous commit-time.
	// Between Commit and the next Begin, no Tx-bound alloc fires;
	// the writer pager's stale highWaterMark / reclamationBound
	// fields are harmless until the next Begin re-seeds them.
	return nil
}

// Rollback discards every change the transaction has made: slab
// buffers go back to the pool, the in-memory bitmap and HighWaterMark
// and RPL chain are restored from the snapshot taken at Begin, and
// tx-scoped bookkeeping is cleared. The on-disk state is unchanged
// (no pwrites occurred). Safe to call on an already-closed tx (returns
// ErrTxClosed without side effects).
//
// Unresolved descendants do not freeze Rollback: the open child chain
// is cascade-rolled-back deepest-first and then this transaction —
// the parent-freeze invariant's one exception (transactions.md
// §Nested Transactions), so a dropped child handle can never strand
// the cross-process write grant.
func (tx *Tx) Rollback() error {
	if tx.closed {
		return ErrTxClosed
	}
	// Rollback cascades through an unresolved descendant chain
	// (deepest-first) instead of freezing (transactions.md §Nested
	// Transactions, parent-freeze invariant): rollback is the
	// abandon-everything operation, and freezing it would leave a
	// caller holding only the parent of a dropped child handle with
	// no API able to release the cross-process write grant until GC.
	// Commit stays frozen — committing over an unresolved child is
	// ambiguous and must be resolved explicitly.
	if tx.activeChild != nil {
		tx.activeChild.cascadeRollback()
	}
	// A child transaction rolls back by restoring its pager savepoint
	// and discarding its keyspace state — the parent is untouched.
	if tx.parent != nil {
		return tx.rollbackChild()
	}
	tx.cleanup.Stop()
	defer tx.releaseGrant()
	tx.closed = true
	tx.endTime = time.Now() // TxStats.Duration: tx open Begin → Rollback
	tx.pgr.AbortTx()
	return nil
}

func (tx *Tx) requireOpen(needsWrite bool) error {
	if tx.closed {
		return ErrTxClosed
	}
	// Parent-freeze (transactions.md §Nested Transactions): a tx with an
	// unresolved child is frozen — every operation, including a read
	// and Commit, returns ErrChildActive until the child resolves.
	// This guards both the public data-op surface and the top-level
	// Commit path. Rollback does not route through requireOpen: it
	// handles activeChild itself by cascading the chain (the freeze's
	// one exception).
	if tx.activeChild != nil {
		return ErrChildActive
	}
	if needsWrite && !tx.writable {
		return ErrReadOnly
	}
	// Use-after-Close graceful-fail. Per leak-detection.md
	// §Close Ordering, Close is not safe concurrent with active
	// transactions in the same process — but a buggy caller still
	// returns ErrClosed rather than SIGSEGV'ing on the now-unmapped
	// mmap. The check is atomic and race-clean; the eventual pager
	// op below this point still races a concurrent Close that
	// hasn't yet stored the gate's closed flag, but tx.pgr (captured at Begin)
	// is a stable Go-heap pointer so the field access itself is
	// race-clean.
	if tx.db.closeGate.IsClosed() {
		return ErrClosed
	}
	return nil
}

// releaseGrant releases the cross-process write grant if this tx
// still holds it. The CAS on tx.held ensures exactly one releaser
// across Commit, Rollback, and the GC cleanup — a leaked-then-
// explicitly-closed tx (or any other double-close race) cannot
// double-warn or double-fire AbortTx. Grant.Release itself is
// sync.Once-guarded; the held atomic only coordinates the leak-
// detection branch.
func (tx *Tx) releaseGrant() {
	if !tx.writable || tx.held == nil {
		return
	}
	if tx.held.CompareAndSwap(true, false) {
		if tx.grant != nil {
			tx.grant.Release()
		}
	}
}

// flushKeyspaces applies the in-memory descriptor state to the
// keyspace B+tree at Commit time. The walk:
//
//  1. For every *Keyspace in openKeyspaces whose state is Created or
//     Dirty (alphabetical by name), encode the descriptor and issue
//     btree.Put.
//  2. For every name in dirtyDescriptors (alphabetical), encode and
//     issue btree.Put. No openKeyspaces / pendingDeletes filter is
//     applied — the disjointness invariant on the tx field godocs
//     guarantees no overlap (every open of a staged name moves the
//     entry into the handle; DeleteKeyspace supersedes it).
//
// pendingDeletes needs no step: DeleteKeyspace removes the descriptor
// row eagerly (the map survives purely as the same-tx semantic
// overlay for opens/creates/lookups).
//
// Budget: every write here is a same-size upsert — descriptors are
// fixed 40-byte values and registry entries only change their
// fixed-width Root/Count fields post-open, while inserts and deletes
// are performed eagerly by Create*/DeleteKeyspace — so no write can
// split or merge, and each costs exactly one CoW page per tree path
// level. That exact cost is held in the pager's commit reserve
// (recalcFlushReserve maintains it at every obligation event), and
// the caller runs this walk inside the pager's commit phase so the
// writes draw from the reserved space (pager-slab.md §Slab Budget).
// A transaction whose ops saw ErrTxTooLarge can therefore always
// commit its applied work.
//
// On failure the caller (Tx.Commit) calls pager.AbortTx — every
// mutation done by this flush rolls back via the bitmap snapshot
// taken at Begin. No on-disk pwrite has happened yet (pager.Commit's
// step-1 runs after this), so AbortTx is strictly sufficient to
// restore pre-flush state.
func (tx *Tx) flushKeyspaces() error {
	// Test injection seam: the reserve machinery makes a real flush
	// failure engineered-unreachable in ordinary fixtures, but the
	// failure PATH (assembly-class commit error: AbortTx, no poison,
	// ErrCommitNotVisible) is contract and needs pinning — same
	// pattern as commitStep4HookForTest.
	if hook := flushFailHookForTest.Load(); hook != nil {
		if err := (*hook)(); err != nil {
			return err
		}
	}
	if len(tx.dirtyDescriptors) == 0 && !tx.hasDirtyOpenKeyspaces() && !tx.hasDirtyOpenSetKeyspaces() {
		return nil
	}
	cfg := tx.pgr.Config()

	// Step 2a: Kind=0 open keyspaces with Created or Dirty state.
	if tx.hasDirtyOpenKeyspaces() {
		names := dirtyOpenNamesSorted(tx.openKeyspaces)
		buf := make([]byte, descriptor.Size)
		for _, name := range names {
			ks := tx.openKeyspaces[unique.Make(name)]
			// Sync the in-memory pinnedIndex root/count back
			// to the registry sub-tree BEFORE encoding the
			// descriptor — registryPut updates ks.desc.IndexRegistryRoot,
			// and we want that final root in the flushed descriptor.
			// Read-only handles skip the sync: they cannot change
			// root/count (the stored entries are already correct),
			// and their registry paths were never pre-paid (the only
			// dirty RO handle is a transferred staged entry, whose
			// stager paid the descriptor path only).
			if !ks.readOnly {
				if err := tx.flushIndexRegistry(ks, ks.indexes); err != nil {
					return fmt.Errorf("flushKeyspaces: index registry sync %q: %w", name, err)
				}
			}
			descriptor.Encode(buf, ks.desc)
			newRoot, err := btree.Put(btreeWriter{tx.pgr}, cfg, tx.keyspaceRoot, []byte(name), buf)
			if err != nil {
				return fmt.Errorf("flushKeyspaces: btree.Put %q: %w", name, mapBtreeErr(err))
			}
			tx.keyspaceRoot = newRoot
			ks.state = keyspaceStateClean
		}
	}

	// Step 2b: Kind=1 open set-keyspaces with Created or Dirty state.
	// Symmetric to 2a — the descriptor encoding is kind-agnostic
	// (descriptor.Encode writes the full struct including
	// Kind + FixedValueSize), so the only difference is the source
	// map and the *SetKeyspace handle type. Sync the
	// SetKeyspace pinnedIndex root/count back to the registry
	// sub-tree BEFORE encoding the descriptor, mirroring Step 2a.
	if tx.hasDirtyOpenSetKeyspaces() {
		names := dirtySetOpenNamesSorted(tx.openSetKeyspaces)
		buf := make([]byte, descriptor.Size)
		for _, name := range names {
			sks := tx.openSetKeyspaces[unique.Make(name)]
			if !sks.readOnly {
				if err := tx.flushIndexRegistry(sks, sks.indexes); err != nil {
					return fmt.Errorf("flushKeyspaces: index registry sync %q (SetKeyspace): %w", name, err)
				}
			}
			descriptor.Encode(buf, sks.desc)
			newRoot, err := btree.Put(btreeWriter{tx.pgr}, cfg, tx.keyspaceRoot, []byte(name), buf)
			if err != nil {
				return fmt.Errorf("flushKeyspaces: btree.Put %q (SetKeyspace): %w", name, mapBtreeErr(err))
			}
			tx.keyspaceRoot = newRoot
			sks.state = keyspaceStateClean
		}
	}

	// Step 3: dirty descriptors not owned by an open *Keyspace.
	if len(tx.dirtyDescriptors) > 0 {
		names := make([]string, 0, len(tx.dirtyDescriptors))
		for name := range tx.dirtyDescriptors {
			names = append(names, name)
		}
		sort.Strings(names)
		buf := make([]byte, descriptor.Size)
		for _, name := range names {
			desc := tx.dirtyDescriptors[name]
			descriptor.Encode(buf, desc)
			newRoot, err := btree.Put(btreeWriter{tx.pgr}, cfg, tx.keyspaceRoot, []byte(name), buf)
			if err != nil {
				return fmt.Errorf("flushKeyspaces: btree.Put (dirty descriptor) %q: %w", name, mapBtreeErr(err))
			}
			tx.keyspaceRoot = newRoot
		}
	}
	return nil
}

// ensureKeyspacePathLen populates the ksPathLen cache if it has not
// been measured this tx. Called from every error-returning site that
// can create a flush obligation (opens, creates, staging writers,
// DeleteKeyspace), so the arithmetic-only recalcFlushReserve — and
// markDirty's hot-path transition — never perform I/O.
func (tx *Tx) ensureKeyspacePathLen() error {
	if tx.ksPathLen > 0 {
		return nil
	}
	return tx.refreshKeyspacePathLen()
}

// refreshKeyspacePathLen re-measures the keyspace B+tree's path
// length. Called after the eager keyspace DDL writes (create insert /
// delete removal) — the only operations that can change the tree's
// height; the same-size upserts everything else performs cannot.
func (tx *Tx) refreshKeyspacePathLen() error {
	if tx.keyspaceRoot == 0 {
		// Empty tree: the next eager create insert allocates exactly
		// the root leaf; one page is the correct flush-write cost.
		tx.ksPathLen = 1
		return nil
	}
	n, err := btree.PathLen(tx.pgr, tx.pgr.Config(), tx.keyspaceRoot)
	if err != nil {
		return mapBtreeErr(err)
	}
	if n < 1 {
		n = 1
	}
	tx.ksPathLen = n
	return nil
}

// measureRegPathLen (re)measures the registry sub-tree path length
// feeding c's commit-flush registry charge. Called at open/create and
// after registry DDL (Rebuild/Drop).
func (tx *Tx) measureRegPathLen(c *keyspaceCore) error {
	if c.desc.IndexRegistryRoot == 0 {
		c.regPathLen = 0
		return nil
	}
	n, err := btree.PathLen(tx.pgr, tx.pgr.Config(), c.desc.IndexRegistryRoot)
	if err != nil {
		return mapBtreeErr(err)
	}
	c.regPathLen = n
	return nil
}

// checkReserveAffordable is the admission gate for OBLIGATION events:
// after an event raises the commit reserve, the raiser calls this and
// unwinds the event on ErrTxTooLarge, so the RESERVE — the slab the
// commit phase itself must allocate (descriptor flush + RPL
// segments), which cannot spill — always fits the budget. Live
// dirtyBytes is deliberately not charged: data pages spill at
// operation boundaries (pager-slab.md §Slab Budget, spill threshold),
// so they claim no commit-phase headroom.
func (tx *Tx) checkReserveAffordable() error {
	if tx.pgr.CommitReserveBytes() > tx.pgr.MaxBytes() {
		return ErrTxTooLarge
	}
	return nil
}

// recalcFlushReserve recomputes the pager's external commit reserve —
// the exact slab cost of Tx.Commit's descriptor flush — and installs
// it via SetExternalReserve so ops-phase admission always leaves the
// flush affordable. Pure arithmetic over cached path lengths
// (ksPathLen / per-handle regPathLen); no I/O, so it is safe on the
// markDirty hot path. Called at every event that changes the flush
// obligation set or a cached path length.
//
// The projection is an exact upper bound: every flush write is a
// same-size upsert (no splits/merges — inserts and deletes are
// eager), costing one CoW page per path level; the trailing term
// over-covers the RPL segment growth the flush's own retires can
// cause (at most one entry per flush-CoW'd page).
func (tx *Tx) recalcFlushReserve() {
	if !tx.writable || tx.pgr == nil {
		return
	}
	pages := 0
	for _, ks := range tx.openKeyspaces {
		// reserveCharged counts a still-Clean handle whose mutator
		// passed the requireWritable pre-charge: the obligation is
		// admitted before the op's own allocations consume the
		// headroom it needs (INV-COMMIT-HEADROOM obligation edge).
		if ks.dead || (ks.state == keyspaceStateClean && !ks.reserveCharged) {
			continue
		}
		pages += tx.ksPathLen
		if !ks.readOnly && len(ks.indexes) > 0 {
			pages += len(ks.indexes) * ks.regPathLen
		}
	}
	for _, sks := range tx.openSetKeyspaces {
		if sks.dead || (sks.state == keyspaceStateClean && !sks.reserveCharged) {
			continue
		}
		pages += tx.ksPathLen
		if !sks.readOnly && len(sks.indexes) > 0 {
			pages += len(sks.indexes) * sks.regPathLen
		}
	}
	pages += len(tx.dirtyDescriptors) * tx.ksPathLen
	if pages > 0 {
		cfg := tx.pgr.Config()
		if capPerSeg := pager.RPLEntriesPerSegment(cfg); capPerSeg > 0 {
			pages += (pages + capPerSeg - 1) / capPerSeg
		}
	}
	tx.pgr.SetExternalReserve(pages * int(tx.pgr.Config().PageSize))
}

// hasDirtyOpenKeyspaces reports whether any open *Keyspace handle has
// pending descriptor state that the flush walk must persist.
func (tx *Tx) hasDirtyOpenKeyspaces() bool {
	for _, ks := range tx.openKeyspaces {
		if ks.state != keyspaceStateClean {
			return true
		}
	}
	return false
}

// hasDirtyOpenSetKeyspaces reports whether any open *SetKeyspace
// handle has pending descriptor state. Kind=1 partner of
// hasDirtyOpenKeyspaces.
func (tx *Tx) hasDirtyOpenSetKeyspaces() bool {
	for _, sks := range tx.openSetKeyspaces {
		if sks.state != keyspaceStateClean {
			return true
		}
	}
	return false
}

// dirtyOpenNamesSorted returns the names of openKeyspaces entries
// touchedNotifySlots collects the notification slots of every
// keyspace this transaction touched — dirty/created open handles of
// both kinds, unopened staged descriptor mutations, deletions, and
// dead (deleted) handles — deduplicated (names may hash-collide) and
// sorted. Must run BEFORE flushKeyspaces marks the handles clean.
//
// The dead-handle lists are load-bearing, not redundant with
// pendingDeletes: a delete→recreate→delete of a pre-existing name
// consumes the pendingDeletes marker on the recreate and the second
// delete leaves only a dead created-state handle — yet the commit's
// net peer-visible effect is that deletion. Scanning the dead lists
// over-notifies a purely created-then-deleted name (never peer
// visible); that is a spurious wake the wait contract allows,
// whereas a missed deletion is a contract violation.
func (tx *Tx) touchedNotifySlots() []uint32 {
	seen := make(map[uint32]struct{})
	add := func(name string) {
		seen[lock.KeyspaceNotifySlot(name)] = struct{}{}
	}
	for _, ks := range tx.openKeyspaces {
		if ks.state != keyspaceStateClean {
			add(ks.name.Value())
		}
	}
	for _, sks := range tx.openSetKeyspaces {
		if sks.state != keyspaceStateClean {
			add(sks.name.Value())
		}
	}
	for name := range tx.dirtyDescriptors {
		add(name)
	}
	for name := range tx.pendingDeletes {
		add(name)
	}
	for _, ks := range tx.deadKeyspaces {
		add(ks.name.Value())
	}
	for _, sks := range tx.deadSetKeyspaces {
		add(sks.name.Value())
	}
	slots := make([]uint32, 0, len(seen))
	for s := range seen {
		slots = append(slots, s)
	}
	sort.Slice(slots, func(i, j int) bool { return slots[i] < slots[j] })
	return slots
}

// publishCommitNotify stamps the notification region for a commit
// that has become visible (success path, and the classified
// visible/durability-unknown failure paths). Caller MUST still hold
// the write grant — the grant both serializes the version stamps and
// (by blocking DB.Close's shutdown-checkpoint acquisition) keeps the
// lock-file mapping alive; the Ref bridges the poisoned-Close case,
// where Close skips that acquisition. A nil lockFile (read-only
// media, or a Close that already tore down state) skips the publish:
// peers then have no notification region mapped either.
func (tx *Tx) publishCommitNotify(slots []uint32) {
	tx.db.mu.Lock()
	lf := tx.db.lockFile
	if lf != nil {
		// Safe under db.mu: Close nils db.lockFile (under this mutex)
		// strictly before dropping the owning reference.
		lf.Ref()
	}
	tx.db.mu.Unlock()
	if lf == nil {
		return
	}
	lf.PublishCommit(slots)
	_ = lf.Close() // drop the Ref
}

// whose state is Created or Dirty, sorted for deterministic flush
// ordering.
func dirtyOpenNamesSorted(m map[uniqueNameHandle]*Keyspace) []string {
	out := make([]string, 0, len(m))
	for _, ks := range m {
		if ks.state == keyspaceStateClean {
			continue
		}
		out = append(out, ks.name.Value())
	}
	sort.Strings(out)
	return out
}

// dirtySetOpenNamesSorted is the Kind=1 partner of
// dirtyOpenNamesSorted — same shape, different value type.
func dirtySetOpenNamesSorted(m map[uniqueNameHandle]*SetKeyspace) []string {
	out := make([]string, 0, len(m))
	for _, sks := range m {
		if sks.state == keyspaceStateClean {
			continue
		}
		out = append(out, sks.name.Value())
	}
	sort.Strings(out)
	return out
}

// mapPagerErr translates pager package sentinels to the root package's
// public sentinels. Other errors pass through verbatim.
//
// ErrCorrupted is wrapped (not replaced) so the descriptive pager-side
// message ("RPL chain cycle at page N", "meta0 PageSize invalid", etc.)
// is preserved in the error chain; callers using errors.Is satisfy
// both gmdb.ErrCorrupted and pager.ErrCorrupted.
func mapPagerErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, pager.ErrReadOnly):
		return ErrReadOnly
	case errors.Is(err, pager.ErrTxTooLarge):
		// Wrapped, not replaced: the degraded-spill context (the
		// admitInstall arm names the recorded spill failure) must
		// survive to the public surface.
		return fmt.Errorf("%w: %w", ErrTxTooLarge, err)
	case errors.Is(err, pager.ErrDBFull):
		return ErrDBFull
	case errors.Is(err, pager.ErrBadPageChecksum):
		return fmt.Errorf("%w: %w", ErrBadPageChecksum, err)
	case errors.Is(err, pager.ErrVersionMismatch):
		return fmt.Errorf("%w: %w", ErrVersionMismatch, err)
	case errors.Is(err, pager.ErrCorrupted):
		return fmt.Errorf("%w: %w", ErrCorrupted, err)
	default:
		return fmt.Errorf("gmdb: %w", err)
	}
}

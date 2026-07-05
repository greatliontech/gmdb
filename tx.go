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

	"github.com/thegrumpylion/gmdb/internal/btree"
	"github.com/thegrumpylion/gmdb/internal/lock"
	"github.com/thegrumpylion/gmdb/internal/page"
	"github.com/thegrumpylion/gmdb/internal/pager"
)

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
	// is gated by db.closed checks at method entry rather than by
	// re-reading db.pgr.
	pgr *pager.Pager

	prevMeta   page.Meta
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
	// SetSizeParams reloads the pager from it).
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
	dirtyDescriptors map[string]page.KeyspaceDescriptor

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
// Captures the shared *closeGate by pointer (leak-detection.md
// clause-explicit invariant — required because runtime.AddCleanup
// provides no ordering between the DB cleanup and Tx cleanups, and
// the gate was promoted from a plain *atomic.Bool to a
// *closeGate with an additional inflight-cleanup refcount so
// Close can drain in-flight cleanups before unmap). Also captures
// *Pager and *Grant directly (not via *DB) so a concurrent
// DB.Close — which sets db.pgr = nil and db.coord = nil — does not
// nil-deref this callback.
type txCleanupInfo struct {
	gate      *closeGate
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
// invariants): observing `*db.closed == true` MUST return without
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
	tx.pgr.SetCommitPhase(true)
	if err := tx.flushKeyspaces(); err != nil {
		// Flush failed before pager.Commit ran — AbortTx is sufficient.
		// No on-disk pwrite has happened yet (pager.Commit's step-1
		// runs later), so no DB-wide poisoning. The caller can retry
		// in a fresh tx.
		tx.pgr.SetCommitPhase(false)
		tx.pgr.AbortTx()
		return err
	}
	// Compose Flags + SyncPolicy per durability.md §Durability
	// Modes. The PageChecksum bit is immutable across commits
	// (carried forward from prev); the Checkpoint bit is computed
	// per SyncMode policy:
	//   - SyncDurable / SyncDataOnly: SET Checkpoint (data IS
	//     durable post-step-2 fsync; meta MAY not be durable in
	//     DataOnly mode but the previous meta is — recovery picks
	//     whichever survives).
	//   - SyncLazy: CLEAR Checkpoint (data MAY NOT be durable;
	//     recovery's checkpoint-preferring selector will fall back
	//     to the last checkpoint-flagged meta).
	flags := tx.prevMeta.Flags & page.MetaFlagPageChecksum
	syncPolicy := pager.SyncBoth
	switch tx.db.opts.SyncMode {
	case SyncDurable:
		flags |= page.MetaFlagCheckpoint
		syncPolicy = pager.SyncBoth
	case SyncDataOnly:
		flags |= page.MetaFlagCheckpoint
		syncPolicy = pager.SyncDataOnly
	case SyncLazy:
		// Checkpoint NOT set.
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
		if !errors.Is(err, pager.ErrTxTooLarge) && !errors.Is(err, pager.ErrDBFull) {
			tx.db.poisoned.Store(true)
		}
		return mapPagerErr(err)
	}
	tx.db.mu.Lock()
	tx.db.currentMeta = result.Meta
	tx.db.activeMetaIdx = result.ActiveMetaIdx
	// A checkpoint commit (SyncDurable/SyncDataOnly set MetaFlagCheckpoint)
	// advances the RPL reclamation bound (free-space.md §RPL Reclamation); a
	// SyncLazy commit leaves the checkpoint flag clear and the
	// bound unchanged, so reclamation never frees pages a recoverable
	// checkpoint meta's tree references.
	if result.Meta.HasFlag(page.MetaFlagCheckpoint) {
		tx.db.lastCheckpointTxnID = result.Meta.TxnID
	}
	tx.db.mu.Unlock()
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
	// hasn't yet stored db.closed, but tx.pgr (captured at Begin)
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
	if len(tx.dirtyDescriptors) == 0 && !tx.hasDirtyOpenKeyspaces() && !tx.hasDirtyOpenSetKeyspaces() {
		return nil
	}
	cfg := tx.pgr.Config()

	// Step 2a: Kind=0 open keyspaces with Created or Dirty state.
	if tx.hasDirtyOpenKeyspaces() {
		names := dirtyOpenNamesSorted(tx.openKeyspaces)
		buf := make([]byte, page.KeyspaceDescriptorSize)
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
			page.EncodeKeyspaceDescriptor(buf, ks.desc)
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
	// (EncodeKeyspaceDescriptor writes the full struct including
	// Kind + FixedValueSize), so the only difference is the source
	// map and the *SetKeyspace handle type. Sync the
	// SetKeyspace pinnedIndex root/count back to the registry
	// sub-tree BEFORE encoding the descriptor, mirroring Step 2a.
	if tx.hasDirtyOpenSetKeyspaces() {
		names := dirtySetOpenNamesSorted(tx.openSetKeyspaces)
		buf := make([]byte, page.KeyspaceDescriptorSize)
		for _, name := range names {
			sks := tx.openSetKeyspaces[unique.Make(name)]
			if !sks.readOnly {
				if err := tx.flushIndexRegistry(sks, sks.indexes); err != nil {
					return fmt.Errorf("flushKeyspaces: index registry sync %q (SetKeyspace): %w", name, err)
				}
			}
			page.EncodeKeyspaceDescriptor(buf, sks.desc)
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
		buf := make([]byte, page.KeyspaceDescriptorSize)
		for _, name := range names {
			desc := tx.dirtyDescriptors[name]
			page.EncodeKeyspaceDescriptor(buf, desc)
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

// checkReserveAffordable is the admission gate for OBLIGATION events
// (INV-COMMIT-HEADROOM's obligation edge — the pager's allocation
// admission covers the other edge): after an event raises the commit
// reserve, the raiser calls this and unwinds the event on
// ErrTxTooLarge, so `dirtyBytes + commitReserve ≤ MaxTxBufferBytes`
// holds at every point, not just at allocations.
func (tx *Tx) checkReserveAffordable() error {
	if tx.pgr.DirtyBytes()+tx.pgr.CommitReserveBytes() > tx.pgr.MaxBytes() {
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
		if capPerSeg := page.RPLEntriesPerSegment(cfg); capPerSeg > 0 {
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

// sortedKeys returns the keys of m sorted. Used by the flush walk
// for deterministic apply order across pendingDeletes.
func sortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
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
		return ErrTxTooLarge
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

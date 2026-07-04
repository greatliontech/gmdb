package gmdb

import (
	"maps"
	"time"
)

// BeginChild creates a child transaction nested within tx
// (transactions.md §Nested Transactions). The child shares tx's pager
// and the top-level write grant; it can be committed (its changes merge
// into the parent) or rolled back (discarded) independently. Children
// may nest to arbitrary depth.
//
// While the returned child — or any of its descendants — is open and
// unresolved, tx is FROZEN: every operation on tx, including Commit
// and a second BeginChild, returns ErrChildActive until the child
// commits or rolls back (LMDB-style parent-freeze). Rollback is the
// exception — it cascade-rolls-back the open descendant chain
// deepest-first and then tx itself, so a dropped child handle can
// never strand the write grant. The freeze prevents the parent and
// child from racing on the shared copy-on-write pager state.
//
// Handle lifetime. Keyspace / SetKeyspace / Cursor handles opened on the
// child are valid only for the child's lifetime — every child handle
// returns ErrTxClosed once the child commits or rolls back. After a
// child commits, continue through a handle opened on the parent (the
// parent's handles reflect the committed child work; re-open by name if
// the parent never had the keyspace open).
//
// Errors:
//   - ErrTxClosed if tx is already committed / rolled back.
//   - ErrReadOnly — never returned in practice (children exist only for
//     write transactions); a read snapshot is a *ReadTx with no
//     BeginChild method.
//   - ErrChildActive if tx already has an unresolved child open.
func (tx *Tx) BeginChild() (*Tx, error) {
	if err := tx.requireOpen(true); err != nil {
		return nil, err
	}
	sp := tx.pgr.BeginSavepoint()
	child := &Tx{
		db:         tx.db,
		pgr:        tx.pgr,
		prevMeta:   tx.prevMeta,
		prevActive: tx.prevActive,
		newTxnID:   tx.newTxnID,
		writable:   true,
		parent:     tx,
		savepoint:  sp,
		startTime:  time.Now(), // TxStats.Duration anchor (child lifetime)
		// held / grant deliberately nil: only the top-level parent owns
		// the cross-process write grant and releases it on Commit /
		// Rollback. The child never registers a leak-detection cleanup —
		// a leaked child does not own pager teardown; the top-level
		// Commit/Rollback (or its leak cleanup) resolves the pager.
		keyspaceRoot:     tx.keyspaceRoot,
		numKeyspaces:     tx.numKeyspaces,
		dirtyDescriptors: maps.Clone(tx.dirtyDescriptors),
		pendingDeletes:   maps.Clone(tx.pendingDeletes),
	}
	// Clone the parent's in-memory keyspace state so the child sees the
	// parent's uncommitted descriptor mutations (the
	// deferred-flush design keeps them on the handles, not on disk) yet
	// can mutate and roll them back without touching the parent. Done
	// after constructing child so the clones can point their tx at it.
	child.openKeyspaces = cloneKeyspaceHandles(child, tx.openKeyspaces)
	child.openSetKeyspaces = cloneSetKeyspaceHandles(child, tx.openSetKeyspaces)
	tx.activeChild = child
	return child, nil
}

// commitChild merges the child's work into its parent and releases the
// pager savepoint. The child's page-level mutations stay in the shared
// pager (published at the top-level Commit); its keyspace descriptor
// state is reconciled into the parent's handles by name so a parent
// handle the caller still holds reflects the committed child work.
//
// Invoked from Tx.Commit when tx.parent != nil; the caller has already
// passed requireOpen (so the child has no unresolved grandchild). tx.parent
// and tx.savepoint are both non-nil here by construction: only BeginChild
// sets tx.parent, and it sets tx.savepoint in the same statement; once
// resolved tx.closed is true, so requireOpen rejects re-entry before this
// runs.
func (tx *Tx) commitChild() error {
	parent := tx.parent
	tx.closed = true
	tx.endTime = time.Now() // TxStats.Duration: child open BeginChild → Commit

	parent.keyspaceRoot = tx.keyspaceRoot
	parent.numKeyspaces = tx.numKeyspaces
	parent.dirtyDescriptors = tx.dirtyDescriptors
	parent.pendingDeletes = tx.pendingDeletes

	mergeKeyspaceHandles(parent, tx)
	mergeSetKeyspaceHandles(parent, tx)

	// Dead handles the child created migrate to the parent (re-pointed
	// so a post-merge op on them still finds the parent's closed checks).
	for _, dk := range tx.deadKeyspaces {
		dk.tx = parent
	}
	parent.deadKeyspaces = append(parent.deadKeyspaces, tx.deadKeyspaces...)
	for _, dk := range tx.deadSetKeyspaces {
		dk.tx = parent
	}
	parent.deadSetKeyspaces = append(parent.deadSetKeyspaces, tx.deadSetKeyspaces...)

	tx.pgr.ReleaseSavepoint(tx.savepoint)
	tx.savepoint = nil
	parent.activeChild = nil
	return nil
}

// cascadeRollback rolls tx and every still-open descendant back, deepest
// first (LIFO savepoint order), ignoring the parent-freeze. The batch
// coordinator uses it to recover when a closure returns having left a
// nested child (a grandchild of the batch tx) unresolved. After
// cascadeRollback returns, tx and all its descendants are closed and
// tx.parent.activeChild is cleared, so the enclosing transaction is no
// longer frozen.
//
// tx must be a child (tx.parent != nil); callers are the batch
// coordinator (on a closure's child handle) and Tx.Rollback's cascade
// (on its own activeChild — always a child by construction).
func (tx *Tx) cascadeRollback() {
	if tx.activeChild != nil {
		tx.activeChild.cascadeRollback()
	}
	if tx.closed {
		return
	}
	tx.closed = true
	tx.endTime = time.Now() // TxStats.Duration: BeginChild → cascaded rollback
	tx.pgr.RestoreSavepoint(tx.savepoint)
	tx.savepoint = nil
	if tx.parent != nil {
		tx.parent.activeChild = nil
	}
}

// rollbackChild discards the child's work: the pager savepoint is
// restored (page-level mutations reverted, child slab buffers released)
// and the child's keyspace clones are simply dropped — the parent's
// handles and roots were never touched, so no restoration is needed.
//
// Invoked from Tx.Rollback when tx.parent != nil.
func (tx *Tx) rollbackChild() error {
	parent := tx.parent
	tx.closed = true
	tx.endTime = time.Now() // TxStats.Duration: child open BeginChild → Rollback
	tx.pgr.RestoreSavepoint(tx.savepoint)
	tx.savepoint = nil
	parent.activeChild = nil
	return nil
}

// cloneKeyspaceHandles deep-copies a parent's open *Keyspace handles for
// a child transaction: each clone gets its own descriptor + state +
// pinned-index copies and points its tx at the child. openCursors are
// deliberately NOT carried — a parent handle's cursors belong to the
// parent; the child gets fresh cursor tracking. Returns nil for a nil
// source (so a parent that never opened a keyspace produces a nil child
// map, matching the lazy-init pattern elsewhere).
func cloneKeyspaceHandles(child *Tx, src map[uniqueNameHandle]*Keyspace) map[uniqueNameHandle]*Keyspace {
	if src == nil {
		return nil
	}
	out := make(map[uniqueNameHandle]*Keyspace, len(src))
	for h, ks := range src {
		out[h] = &Keyspace{
			keyspaceCore: keyspaceCore{
				tx:       child,
				name:     ks.name,
				desc:     ks.desc,
				state:    ks.state,
				dead:     ks.dead,
				readOnly: ks.readOnly,
				indexes:  clonePinnedIndexes(ks.indexes),
			},
		}
	}
	return out
}

// cloneSetKeyspaceHandles is the Kind=1 partner of
// cloneKeyspaceHandles.
func cloneSetKeyspaceHandles(child *Tx, src map[uniqueNameHandle]*SetKeyspace) map[uniqueNameHandle]*SetKeyspace {
	if src == nil {
		return nil
	}
	out := make(map[uniqueNameHandle]*SetKeyspace, len(src))
	for h, sks := range src {
		out[h] = &SetKeyspace{
			keyspaceCore: keyspaceCore{
				tx:       child,
				name:     sks.name,
				desc:     sks.desc,
				state:    sks.state,
				dead:     sks.dead,
				readOnly: sks.readOnly,
				indexes:  clonePinnedIndexes(sks.indexes),
			},
		}
	}
	return out
}

// clonePinnedIndexes value-copies each *pinnedIndex (the decl pointer is
// shared — IndexDecl is immutable after open; root/count are the mutable
// fields and get fresh storage per clone).
func clonePinnedIndexes(src map[string]*pinnedIndex) map[string]*pinnedIndex {
	if src == nil {
		return nil
	}
	out := make(map[string]*pinnedIndex, len(src))
	for k, pi := range src {
		cp := *pi
		out[k] = &cp
	}
	return out
}

// mergeKeyspaceHandles reconciles a committing child's open *Keyspace
// handles into the parent:
//
//   - A name the parent already has open: the parent's existing handle
//     is updated in place (desc / state / dead / indexes) so a caller
//     still holding that handle observes the committed child work; if
//     the data-tree root moved (copy-on-write changes the root on any
//     mutation), the parent handle's cursors are marked stale.
//   - A name only the child opened/created: a FRESH parent-owned handle
//     carrying the child's descriptor state is installed. The child's own
//     handle object is never promoted — keyspace handles are tx-scoped,
//     so every child handle becomes invalid (ErrTxClosed) when the child
//     resolves, regardless of whether the parent had the name open. A
//     caller continues through a parent handle (re-opening if needed).
//   - A name the parent had open but the child deleted (so it left the
//     child's open set): the parent's handle is invalidated
//     (dead = true) and migrated to deadKeyspaces, matching DeleteKeyspace
//     semantics (api-surface.md §Keyspace API DeleteKeyspace).
//
// The child cloned the parent's full open set at BeginChild, so a name
// absent from the child's open set at commit can only be one the child
// deleted — never one it merely left untouched.
func mergeKeyspaceHandles(parent, child *Tx) {
	for h, cks := range child.openKeyspaces {
		if pks, ok := parent.openKeyspaces[h]; ok {
			rootMoved := pks.desc.Root != cks.desc.Root
			pks.desc = cks.desc
			pks.state = cks.state
			pks.dead = cks.dead
			pks.indexes = cks.indexes
			if rootMoved {
				pks.markCursorsStale()
			}
			continue
		}
		if parent.openKeyspaces == nil {
			parent.openKeyspaces = make(map[uniqueNameHandle]*Keyspace, len(child.openKeyspaces))
		}
		parent.openKeyspaces[h] = &Keyspace{
			keyspaceCore: keyspaceCore{
				tx:       parent,
				name:     cks.name,
				desc:     cks.desc,
				state:    cks.state,
				dead:     cks.dead,
				readOnly: cks.readOnly,
				indexes:  cks.indexes,
			},
		}
	}
	for h, pks := range parent.openKeyspaces {
		if _, stillOpen := child.openKeyspaces[h]; !stillOpen {
			pks.dead = true
			parent.deadKeyspaces = append(parent.deadKeyspaces, pks)
			delete(parent.openKeyspaces, h)
		}
	}
}

// mergeSetKeyspaceHandles is the Kind=1 partner of
// mergeKeyspaceHandles.
func mergeSetKeyspaceHandles(parent, child *Tx) {
	for h, csks := range child.openSetKeyspaces {
		if psks, ok := parent.openSetKeyspaces[h]; ok {
			rootMoved := psks.desc.Root != csks.desc.Root
			psks.desc = csks.desc
			psks.state = csks.state
			psks.dead = csks.dead
			psks.indexes = csks.indexes
			if rootMoved {
				psks.markSetCursorsStale()
			}
			continue
		}
		if parent.openSetKeyspaces == nil {
			parent.openSetKeyspaces = make(map[uniqueNameHandle]*SetKeyspace, len(child.openSetKeyspaces))
		}
		parent.openSetKeyspaces[h] = &SetKeyspace{
			keyspaceCore: keyspaceCore{
				tx:       parent,
				name:     csks.name,
				desc:     csks.desc,
				state:    csks.state,
				dead:     csks.dead,
				readOnly: csks.readOnly,
				indexes:  csks.indexes,
			},
		}
	}
	for h, psks := range parent.openSetKeyspaces {
		if _, stillOpen := child.openSetKeyspaces[h]; !stillOpen {
			psks.dead = true
			parent.deadSetKeyspaces = append(parent.deadSetKeyspaces, psks)
			delete(parent.openSetKeyspaces, h)
		}
	}
}

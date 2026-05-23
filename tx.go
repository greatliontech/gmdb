package gmdb

import (
	"errors"
	"fmt"

	"github.com/thegrumpylion/gmdb/internal/page"
	"github.com/thegrumpylion/gmdb/internal/pager"
)

// Tx is a write transaction. The chunk-1 surface exposes raw page-level
// operations only — Keyspace, Cursor, and the typed layer land in
// later chunks (5+). The byte slices returned by Page / CoW / AllocSlab
// / Mutate are valid until Commit / Rollback completes; do not retain
// them past tx close.
type Tx struct {
	db         *DB
	prevMeta   page.Meta
	prevActive int
	newTxnID   uint64
	writable   bool
	closed     bool
}

// AllocPage allocates a single page following the freespace priority
// (loose → bitmap → RPL reclamation → file extension). The returned id
// is the page ID; the caller typically follows with AllocSlab(id) or
// CoW(src, id) to populate the page's content.
func (tx *Tx) AllocPage() (uint64, error) {
	if err := tx.requireOpen(true); err != nil {
		return 0, err
	}
	tx.db.pgr.SetCurrentTxnID(tx.newTxnID)
	id, err := tx.db.pgr.AllocPage()
	return id, mapPagerErr(err)
}

// FreePage marks id for retirement. Same-tx pages become loose; prior-
// tx pages join the RPL at commit.
func (tx *Tx) FreePage(id uint64) error {
	if err := tx.requireOpen(true); err != nil {
		return err
	}
	return mapPagerErr(tx.db.pgr.FreePage(id))
}

// Page resolves id to a borrowed byte slice. Resolution: slab (own
// writes this tx) → mmap. Slice is valid until tx close.
func (tx *Tx) Page(id uint64) ([]byte, error) {
	if err := tx.requireOpen(false); err != nil {
		return nil, err
	}
	return tx.db.pgr.Page(id), nil
}

// CoW copies the content of srcID into a fresh slab buffer keyed at
// dstID, returning the writable buffer. Idempotent on re-CoW at the
// same dstID (returns the existing buffer).
func (tx *Tx) CoW(srcID, dstID uint64) ([]byte, error) {
	if err := tx.requireOpen(true); err != nil {
		return nil, err
	}
	buf, err := tx.db.pgr.CoW(srcID, dstID)
	return buf, mapPagerErr(err)
}

// AllocSlab installs a fresh zero-filled slab buffer at id and returns
// it. For commit-step-0 assembly (RPL segments, modified bitmap pages);
// also useful for tests that need to fabricate page content without
// reading a source page.
func (tx *Tx) AllocSlab(id uint64) ([]byte, error) {
	if err := tx.requireOpen(true); err != nil {
		return nil, err
	}
	buf, err := tx.db.pgr.AllocSlab(id)
	return buf, mapPagerErr(err)
}

// Mutate returns the slab buffer at id for in-place editing. Returns
// ErrPageNotDirty if id hasn't been CoW'd or AllocSlab'd in this tx.
func (tx *Tx) Mutate(id uint64) ([]byte, error) {
	if err := tx.requireOpen(true); err != nil {
		return nil, err
	}
	buf, err := tx.db.pgr.Mutate(id)
	return buf, mapPagerErr(err)
}

// Commit publishes the transaction's changes via the four-step commit
// protocol (pager-slab.md §Commit Write Ordering). On success the DB's
// active meta advances and the write-lock is released.
func (tx *Tx) Commit() error {
	if err := tx.requireOpen(true); err != nil {
		return err
	}
	defer tx.releaseWriteLock()
	tx.closed = true
	tx.db.pgr.SetCurrentTxnID(tx.newTxnID)
	flags := tx.prevMeta.Flags
	result, err := tx.db.pgr.Commit(pager.CommitParams{
		NewTxnID:     tx.newTxnID,
		KeyspaceRoot: tx.prevMeta.KeyspaceRoot,
		NumKeyspaces: tx.prevMeta.NumKeyspaces,
		Flags:        flags,
	}, tx.prevMeta, tx.prevActive)
	if err != nil {
		return mapPagerErr(err)
	}
	tx.db.mu.Lock()
	tx.db.currentMeta = result.Meta
	tx.db.activeMetaIdx = result.ActiveMetaIdx
	// Re-seed commit state for the next tx.
	tx.db.pgr.SetCommitState(result.Meta.HighWaterMark, result.Meta.MaxSize, result.Meta.TxnID)
	tx.db.mu.Unlock()
	return nil
}

// Rollback discards every change the transaction has made: slab
// buffers go back to the pool, the in-memory bitmap and HighWaterMark
// and RPL chain are restored from the snapshot taken at Begin, and
// tx-scoped bookkeeping is cleared. The on-disk state is unchanged
// (no pwrites occurred). Safe to call on an already-closed tx (returns
// ErrTxClosed without side effects).
func (tx *Tx) Rollback() error {
	if tx.closed {
		return ErrTxClosed
	}
	defer tx.releaseWriteLock()
	tx.closed = true
	tx.db.pgr.AbortTx()
	return nil
}

func (tx *Tx) requireOpen(needsWrite bool) error {
	if tx.closed {
		return ErrTxClosed
	}
	if needsWrite && !tx.writable {
		return ErrReadOnly
	}
	return nil
}

func (tx *Tx) releaseWriteLock() {
	if tx.writable {
		tx.db.writeMu.Unlock()
	}
}

// mapPagerErr translates pager package sentinels to the root package's
// public sentinels. Other errors pass through verbatim.
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
	default:
		return fmt.Errorf("gmdb: %w", err)
	}
}

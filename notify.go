package gmdb

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/greatliontech/gmdb/internal/lock"
	"github.com/greatliontech/gmdb/internal/pager"
)

// Change notification (api-surface.md §Change Notification,
// cross-process.md §Lock File Layout, notification region).
//
// Versions are opaque, monotonically increasing uint64 tokens scoped
// to the database: every committed-visible commit — from any process
// — produces a version greater than every version observed before
// it. Tokens from Version, WaitVersion, and WaitKeyspaceVersion are
// mutually comparable; they are NOT transaction IDs and carry no
// meaning beyond ordering.

// Version returns the database's current commit version: an opaque
// token that increases with every commit that has become visible,
// across processes. Use it as the `from` argument of WaitVersion /
// WaitKeyspaceVersion to wait for subsequent changes.
//
// On a read-only handle without cross-process coordination (read-only
// media; see Options.ReadOnly), the version is derived from the data
// file's committed state.
func (db *DB) Version() (uint64, error) {
	lf, file, cur, err := db.notifySnapshot()
	if err != nil {
		return 0, err
	}
	if lf == nil {
		// Same source the fallback waits poll (the latest committed
		// on-disk meta), so Version and the waits stay consistent
		// even if the "read-only" medium is written out-of-band.
		m, merr := pager.ReadLatestMeta(file, cur.PageSize)
		if merr != nil {
			if db.closeGate.IsClosed() {
				return 0, ErrClosed
			}
			return 0, mapPagerErr(merr)
		}
		return m.TxnID, nil
	}
	defer lf.Close() // drop the Ref taken by notifySnapshot
	return lf.NotifyVersion(lock.NotifyGlobalSlot), nil
}

// WaitVersion blocks until the database's commit version exceeds
// from, returning the observed version. It returns ctx.Err() on
// context cancellation and ErrClosed if the handle is closed while
// waiting. Spurious wakeups are absorbed internally: a successful
// return always means version > from.
//
// A returned version happens-after the publication of the commit
// that produced it: a read transaction opened after WaitVersion
// returns observes that commit.
func (db *DB) WaitVersion(ctx context.Context, from uint64) (uint64, error) {
	return db.waitNotify(ctx, lock.NotifyGlobalSlot, from)
}

// WaitKeyspaceVersion is WaitVersion scoped to commits that touched
// the named keyspace (data writes, creation, configuration,
// deletion). The keyspace need not exist yet — waiting for its
// creation is valid. Scoping is by name hash over a fixed slot
// array, so a colliding unrelated keyspace's commit can wake the
// wait spuriously — but a successful return still always means a
// commit with version > from touched a keyspace in this keyspace's
// hash class; callers re-check the data they care about.
func (db *DB) WaitKeyspaceVersion(ctx context.Context, name string, from uint64) (uint64, error) {
	return db.waitNotify(ctx, lock.KeyspaceNotifySlot(name), from)
}

func (db *DB) waitNotify(ctx context.Context, slot uint32, from uint64) (uint64, error) {
	lf, file, cur, err := db.notifySnapshot()
	if err != nil {
		return 0, err
	}
	if lf == nil {
		return db.waitNotifyPoll(ctx, file, cur, from)
	}
	defer lf.Close() // drop the Ref taken by notifySnapshot
	v, err := lf.WaitNotify(ctx, slot, from, db.closeGate.IsClosed)
	if errors.Is(err, lock.ErrNotifyStopped) {
		return 0, ErrClosed
	}
	return v, err
}

// notifySnapshot captures the notification sources under db.mu. A
// non-nil *lock.File is returned with a lifetime reference taken
// (caller must drop it with Close); nil with a non-nil file means
// the lock-free read-only fallback (poll the data file).
//
// Ref safety: db.lockFile is nil'd under db.mu (DB.Close step 6)
// strictly before the owning reference is dropped (step 8), so a
// non-nil pointer observed under db.mu still holds the owning
// reference and Ref is legal.
func (db *DB) notifySnapshot() (*lock.File, *os.File, pager.Meta, error) {
	if db.closeGate.IsClosed() {
		return nil, nil, pager.Meta{}, ErrClosed
	}
	if db.poisoned.Load() {
		return nil, nil, pager.Meta{}, ErrPoisoned
	}
	db.mu.Lock()
	lf := db.lockFile
	file := db.file
	cur := db.currentMeta
	if lf != nil {
		lf.Ref()
	}
	db.mu.Unlock()
	if file == nil {
		if lf != nil {
			_ = lf.Close()
		}
		return nil, nil, pager.Meta{}, ErrClosed
	}
	return lf, file, cur, nil
}

// notifyPollInterval paces the read-only-media fallback: without a
// lock file there is no notification region, so the waits poll the
// data file's committed meta. On genuinely read-only media the
// version cannot change and the wait simply blocks until its
// context ends.
const notifyPollInterval = 10 * time.Millisecond

// waitNotifyPoll is the lock-free fallback for both wait scopes: it
// polls the latest committed meta's TxnID. Keyspace-scoped waits
// degrade to global waits here — permitted by the spurious-wakeup
// contract (callers re-check).
func (db *DB) waitNotifyPoll(ctx context.Context, file *os.File, cur pager.Meta, from uint64) (uint64, error) {
	for {
		m, err := pager.ReadLatestMeta(file, cur.PageSize)
		if err != nil {
			if db.closeGate.IsClosed() {
				return 0, ErrClosed
			}
			return 0, mapPagerErr(err)
		}
		if m.TxnID > from {
			return m.TxnID, nil
		}
		if db.closeGate.IsClosed() {
			return 0, ErrClosed
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(notifyPollInterval):
		}
	}
}

package lock

import (
	"context"
	"errors"
	"fmt"

	"github.com/zeebo/xxh3"
)

// Notification region protocol (cross-process.md §Lock File Layout,
// notification region). The region's words are opaque, monotonic
// commit-version values, valid for comparison with each other for
// the lifetime of the lock file:
//
//   - Word NotifyGlobalSlot holds the global version: bumped
//     (atomic +1) by every commit's publication, CAS-max seeded from
//     the data file's meta TxnID at every Open so versions keep
//     ascending across a lock-file recreation on an uncompacted
//     database. It is NOT the meta TxnID: Compact resets the TxnID,
//     and a version word must never regress — a waiter compares
//     `value > from`, so a regression would hide every later commit
//     from it.
//   - Words 1..NotifyKeyspaceSlots are stamped with the just-bumped
//     GLOBAL version by commits that touched a keyspace hashing to
//     the word (KeyspaceNotifySlot). Stamping the global value —
//     rather than counting per-slot — keeps every word comparable
//     with every Version() observation: slot > from ⇔ some commit
//     newer than from touched (a keyspace colliding with) the slot.
//
// Writers are serialized by the cross-process write grant, so the
// global bump and the keyspace stamps need no CAS; the Open-time
// seed races with peers and uses CAS-max. Waiters tolerate spurious
// wakeups by contract (hash collisions, unrelated futex wakes) and
// re-check `value > from` before returning.

// ErrNotifyStopped reports that a WaitNotify call was ended by its
// stopped callback (the owning handle is closing) rather than by the
// context or a version change.
var ErrNotifyStopped = errors.New("lock: notify wait stopped")

// KeyspaceNotifySlot maps a keyspace name to its notification word
// index in [1, NotifyKeyspaceSlots]. Distinct names may collide; a
// collision only widens the wake set (spurious wakeups are allowed).
func KeyspaceNotifySlot(name string) uint32 {
	return 1 + uint32(xxh3.HashString(name)%uint64(NotifyKeyspaceSlots))
}

// NotifyVersion returns the current value of a notification word.
// Panics on a closed *File or an out-of-range slot.
func (f *File) NotifyVersion(slot uint32) uint64 {
	return Load64(f.notifyWord(slot))
}

// SeedNotifyVersion raises the global version word to at least v
// (CAS-max; never lowers it). Called by every Open with the adopted
// meta's TxnID, racing freely with peer seeds and grant-serialized
// publishes — max-only writes preserve monotonicity under any
// interleaving.
func (f *File) SeedNotifyVersion(v uint64) {
	w := f.notifyWord(NotifyGlobalSlot)
	for {
		cur := Load64(w)
		if cur >= v || CAS64(w, cur, v) {
			return
		}
	}
}

// PublishCommit bumps the global version word, stamps the new value
// into each given keyspace word, and wakes waiters on every touched
// word. Returns the new global version.
//
// MUST be called under the cross-process write grant (writes are
// plain stores relying on grant serialization for monotonicity), and
// only AFTER the commit's meta publication: a waiter woken by the
// stamp immediately reads the database and must observe the commit —
// its wake is consumed either way.
func (f *File) PublishCommit(keyspaceSlots []uint32) uint64 {
	w := f.notifyWord(NotifyGlobalSlot)
	v := Add64(w, 1)
	notifyWake(w)
	for _, s := range keyspaceSlots {
		if s == NotifyGlobalSlot || s >= NotifySlotCount {
			panic(fmt.Sprintf("lock: PublishCommit keyspace slot %d out of range [1, %d)", s, NotifySlotCount))
		}
		kw := f.notifyWord(s)
		Store64(kw, v)
		notifyWake(kw)
	}
	return v
}

// WaitNotify blocks until the notification word at slot exceeds
// from, the context is done, or stopped reports true (checked at
// least every notify wait slice; pass nil for no stop condition).
// Returns the observed value on success, the context's error, or
// ErrNotifyStopped.
//
// The caller must hold a lifetime reference on the mapping (Ref) for
// the full duration of the call — the wait touches the mmap'd word
// from both this goroutine and, on context cancellation, the
// AfterFunc wake below.
func (f *File) WaitNotify(ctx context.Context, slot uint32, from uint64, stopped func() bool) (uint64, error) {
	w := f.notifyWord(slot)
	// Instant context responsiveness on the futex path: cancellation
	// wakes the word so the loop re-checks immediately instead of
	// waiting out the slice. Peers woken by it re-check and re-sleep
	// (spurious wakeups allowed). The done channel closes the
	// callback-vs-return race: if the callback already started, wait
	// for it — it must not touch the word after the caller's Ref is
	// dropped.
	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		defer close(done)
		notifyWake(w)
	})
	defer func() {
		if !stop() {
			<-done
		}
	}()
	wait := notifyWaitState{}
	for {
		v := Load64(w)
		if v > from {
			return v, nil
		}
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		if stopped != nil && stopped() {
			return 0, ErrNotifyStopped
		}
		wait.sleep(w, v)
	}
}

func (f *File) notifyWord(slot uint32) *uint64 {
	if f.notify == nil {
		panic("lock: notify access on closed *File")
	}
	if slot >= NotifySlotCount {
		panic(fmt.Sprintf("lock: notify slot %d out of range [0, %d)", slot, NotifySlotCount))
	}
	return &f.notify[slot]
}

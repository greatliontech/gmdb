package gmdb

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/greatliontech/gmdb/internal/lock"
	"github.com/greatliontech/gmdb/internal/pager"
)

// Change notification (api-surface.md §Change Notification): opaque
// monotonic versions, global and keyspace-scoped blocking waits,
// cross-handle wakes through the lock file's notification region.

func notifyOpen(t *testing.T, path string) *DB {
	t.Helper()
	db, err := Open(context.Background(), path, Options{
		PageSize: 4096, MinSize: 16, MaxSize: 256,
		Maintenance: MaintenanceOptions{Disable: true},
	})
	if err != nil {
		t.Fatalf("Open(%s): %v", path, err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func notifyCommit(t *testing.T, db *DB, ks, key, val string) {
	t.Helper()
	tx, err := db.Begin(context.Background())
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	k, err := tx.CreateKeyspaceIfNotExists(ks)
	if err != nil {
		t.Fatalf("CreateKeyspaceIfNotExists(%s): %v", ks, err)
	}
	if err := k.Put([]byte(key), []byte(val)); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
}

func TestVersionAdvancesOnCommit(t *testing.T) {
	db := notifyOpen(t, tmpPath(t))
	v0, err := db.Version()
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	notifyCommit(t, db, "k", "a", "1")
	v1, err := db.Version()
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if v1 <= v0 {
		t.Fatalf("version did not advance across a commit: %d then %d", v0, v1)
	}
	notifyCommit(t, db, "k", "a", "2")
	v2, err := db.Version()
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if v2 <= v1 {
		t.Fatalf("version did not advance across a second commit: %d then %d", v1, v2)
	}
}

// TestWaitVersionWakesOnPeerCommit: the global wait's cross-handle
// contract — a waiter on handle A is woken by handle B's commit, and
// a read transaction opened after the wake observes that commit
// (publish happens-before stamp).
func TestWaitVersionWakesOnPeerCommit(t *testing.T) {
	path := tmpPath(t)
	a := notifyOpen(t, path)
	b := notifyOpen(t, path)

	from, err := a.Version()
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	type res struct {
		v   uint64
		err error
	}
	ch := make(chan res, 1)
	go func() {
		v, err := a.WaitVersion(context.Background(), from)
		ch <- res{v, err}
	}()
	time.Sleep(20 * time.Millisecond)
	notifyCommit(t, b, "k", "hello", "world")
	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatalf("WaitVersion: %v", r.err)
		}
		if r.v <= from {
			t.Fatalf("WaitVersion returned %d, not > from %d", r.v, from)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("WaitVersion not woken by peer commit within 5s")
	}
	// Happens-after: the woken waiter's next read sees the commit.
	rtx, err := a.BeginRead(context.Background())
	if err != nil {
		t.Fatalf("BeginRead: %v", err)
	}
	defer rtx.Rollback()
	ks, err := rtx.OpenKeyspaceReadOnly("k")
	if err != nil {
		t.Fatalf("Keyspace after wake: %v", err)
	}
	v, err := ks.Get([]byte("hello"))
	if err != nil || string(v) != "world" {
		t.Fatalf("Get after wake = (%q, %v), want (world, nil)", v, err)
	}
}

// notifyDisjointName returns a keyspace name whose notification slot
// differs from every name in avoid — hash collisions are legal but
// would make a scoped-isolation test vacuous.
func notifyDisjointName(t *testing.T, prefix string, avoid ...string) string {
	t.Helper()
	used := map[uint32]bool{}
	for _, a := range avoid {
		used[lock.KeyspaceNotifySlot(a)] = true
	}
	for i := range 1000 {
		n := prefix + string(rune('a'+i%26)) + string(rune('0'+i/26))
		if !used[lock.KeyspaceNotifySlot(n)] {
			return n
		}
	}
	t.Fatal("no collision-free name found")
	return ""
}

// TestWaitKeyspaceVersionScoped: a keyspace-scoped waiter is not
// woken by commits to a keyspace in a different hash slot, and is
// woken — with a version greater than its from — by a commit
// touching its keyspace. The negative half is deterministic, not a
// timing bet: an untouched slot's word never changes, so the waiter
// CANNOT return regardless of scheduling.
func TestWaitKeyspaceVersionScoped(t *testing.T) {
	path := tmpPath(t)
	a := notifyOpen(t, path)
	b := notifyOpen(t, path)

	watched := "watched"
	other := notifyDisjointName(t, "other-", watched)

	from, err := a.Version()
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	type res struct {
		v   uint64
		err error
	}
	ch := make(chan res, 1)
	go func() {
		v, err := a.WaitKeyspaceVersion(context.Background(), watched, from)
		ch <- res{v, err}
	}()
	// Unrelated-keyspace commits must not wake the scoped waiter.
	notifyCommit(t, b, other, "x", "1")
	notifyCommit(t, b, other, "x", "2")
	time.Sleep(150 * time.Millisecond)
	select {
	case r := <-ch:
		t.Fatalf("scoped waiter woke on unrelated-keyspace commit: (%d, %v)", r.v, r.err)
	default:
	}
	// A commit touching the watched keyspace wakes it.
	notifyCommit(t, b, watched, "y", "1")
	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatalf("WaitKeyspaceVersion: %v", r.err)
		}
		if r.v <= from {
			t.Fatalf("WaitKeyspaceVersion returned %d, not > from %d", r.v, from)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("scoped waiter not woken by watched-keyspace commit within 5s")
	}
}

// TestWaitKeyspaceVersionCreation: waiting on a keyspace that does
// not exist yet is valid — creation is a touching commit.
func TestWaitKeyspaceVersionCreation(t *testing.T) {
	path := tmpPath(t)
	a := notifyOpen(t, path)
	b := notifyOpen(t, path)
	from, err := a.Version()
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	ch := make(chan error, 1)
	go func() {
		_, err := a.WaitKeyspaceVersion(context.Background(), "not-yet", from)
		ch <- err
	}()
	time.Sleep(20 * time.Millisecond)
	notifyCommit(t, b, "not-yet", "k", "v")
	select {
	case err := <-ch:
		if err != nil {
			t.Fatalf("WaitKeyspaceVersion: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("creation commit did not wake the waiter within 5s")
	}
}

func TestWaitVersionContextCancel(t *testing.T) {
	db := notifyOpen(t, tmpPath(t))
	from, err := db.Version()
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan error, 1)
	go func() {
		_, err := db.WaitVersion(ctx, from)
		ch <- err
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-ch:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("WaitVersion after cancel = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancellation did not unblock WaitVersion within 5s")
	}
}

// TestWaitVersionUnblocksOnClose: DB.Close ends in-flight waits with
// ErrClosed (bounded by the notify wait slice) instead of stranding
// them; the lock-file mapping stays alive for the waiter via its
// lifetime reference.
func TestWaitVersionUnblocksOnClose(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{
		PageSize: 4096, MinSize: 16, MaxSize: 256,
		Maintenance: MaintenanceOptions{Disable: true},
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	from, err := db.Version()
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	ch := make(chan error, 1)
	go func() {
		_, err := db.WaitVersion(ctx, from)
		ch <- err
	}()
	time.Sleep(20 * time.Millisecond)
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case err := <-ch:
		if !errors.Is(err, ErrClosed) {
			t.Fatalf("WaitVersion after Close = %v, want ErrClosed", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not unblock WaitVersion within 5s")
	}
}

// TestVersionSurvivesLockFileRecreation: versions keep ascending when
// the transient lock file is deleted between opens — the global word
// is CAS-max re-seeded from the data file's committed TxnID.
func TestVersionSurvivesLockFileRecreation(t *testing.T) {
	path := tmpPath(t)
	db := notifyOpen(t, path)
	notifyCommit(t, db, "k", "a", "1")
	notifyCommit(t, db, "k", "a", "2")
	v1, err := db.Version()
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	lockPath := filepath.Join(filepath.Dir(path), lock.BaseFor(filepath.Base(path)))
	if err := os.Remove(lockPath); err != nil {
		t.Fatalf("remove lock file: %v", err)
	}
	db2 := notifyOpen(t, path)
	v2, err := db2.Version()
	if err != nil {
		t.Fatalf("Version after recreation: %v", err)
	}
	if v2 < v1 {
		t.Fatalf("version regressed across lock-file recreation: %d then %d", v1, v2)
	}
	notifyCommit(t, db2, "k", "a", "3")
	v3, err := db2.Version()
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if v3 <= v2 {
		t.Fatalf("version did not advance after recreation: %d then %d", v2, v3)
	}
}

// TestNoOpVerbCommitDoesNotStampKeyspace: a commit whose only
// keyspace operation was a presence-gated no-op (Insert on an
// existing key) bumps the global version — a commit happened — but
// must not stamp the keyspace's slot: nothing about the keyspace
// changed, so scoped waiters stay asleep.
func TestNoOpVerbCommitDoesNotStampKeyspace(t *testing.T) {
	path := tmpPath(t)
	db := notifyOpen(t, path)
	notifyCommit(t, db, "k", "a", "1")

	from, err := db.Version()
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	ch := make(chan struct{}, 1)
	go func() {
		_, _ = db.WaitKeyspaceVersion(context.Background(), "k", from)
		ch <- struct{}{}
	}()

	tx, err := db.Begin(context.Background())
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	ks, err := tx.OpenKeyspace("k")
	if err != nil {
		t.Fatalf("OpenKeyspace: %v", err)
	}
	if err := ks.Insert([]byte("a"), []byte("x")); !errors.Is(err, ErrKeyExists) {
		t.Fatalf("Insert present = %v, want ErrKeyExists", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	time.Sleep(150 * time.Millisecond)
	select {
	case <-ch:
		t.Fatal("no-op-verb commit stamped the keyspace slot and woke the scoped waiter")
	default:
	}
	// Cleanup: wake the parked waiter so the goroutine exits.
	notifyCommit(t, db, "k", "b", "1")
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("cleanup commit did not wake the waiter")
	}
}

// waitKeyspace parks a scoped waiter and returns a receive-only
// result channel.
func waitKeyspace(db *DB, name string, from uint64) <-chan error {
	ch := make(chan error, 1)
	go func() {
		_, err := db.WaitKeyspaceVersion(context.Background(), name, from)
		ch <- err
	}()
	return ch
}

func expectWake(t *testing.T, ch <-chan error, what string) {
	t.Helper()
	select {
	case err := <-ch:
		if err != nil {
			t.Fatalf("%s: waiter error: %v", what, err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("%s: scoped waiter not woken within 5s", what)
	}
}

// TestWaitKeyspaceVersionDeletion: deleting a keyspace is a touching
// commit (api-surface.md §Change Notification) — scoped waiters wake.
func TestWaitKeyspaceVersionDeletion(t *testing.T) {
	path := tmpPath(t)
	a := notifyOpen(t, path)
	b := notifyOpen(t, path)
	notifyCommit(t, b, "k", "x", "1")
	from, err := a.Version()
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	ch := waitKeyspace(a, "k", from)
	time.Sleep(20 * time.Millisecond)
	tx, err := b.Begin(context.Background())
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := tx.DeleteKeyspace("k"); err != nil {
		t.Fatalf("DeleteKeyspace: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	expectWake(t, ch, "plain deletion")
}

// TestWaitKeyspaceVersionDeleteRecreateDelete: a same-tx
// delete→recreate→delete of a PRE-EXISTING keyspace nets out to a
// peer-visible deletion, but consumes the pending-delete marker on
// the recreate and leaves only a dead created-state handle — the one
// touched-keyspace source that is not a live overlay. Scoped waiters
// must still wake (regression: the collector missed the dead-handle
// lists and the waiter slept forever).
func TestWaitKeyspaceVersionDeleteRecreateDelete(t *testing.T) {
	path := tmpPath(t)
	a := notifyOpen(t, path)
	b := notifyOpen(t, path)
	notifyCommit(t, b, "k", "x", "1")
	from, err := a.Version()
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	ch := waitKeyspace(a, "k", from)
	time.Sleep(20 * time.Millisecond)
	tx, err := b.Begin(context.Background())
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := tx.DeleteKeyspace("k"); err != nil {
		t.Fatalf("first DeleteKeyspace: %v", err)
	}
	if _, err := tx.CreateKeyspace("k"); err != nil {
		t.Fatalf("recreate: %v", err)
	}
	if err := tx.DeleteKeyspace("k"); err != nil {
		t.Fatalf("second DeleteKeyspace: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	// The commit's net effect is peer-visible: "k" is gone.
	rtx, err := b.BeginRead(context.Background())
	if err != nil {
		t.Fatalf("BeginRead: %v", err)
	}
	if _, kerr := rtx.OpenKeyspaceReadOnly("k"); !errors.Is(kerr, ErrNotFound) {
		t.Fatalf("post-commit open = %v, want ErrNotFound", kerr)
	}
	rtx.Rollback()
	expectWake(t, ch, "delete-recreate-delete")
}

// TestWaitKeyspaceVersionConfigChange: a configuration-only change on
// an UNOPENED name (the dirtyDescriptors staging source) is a
// touching commit.
func TestWaitKeyspaceVersionConfigChange(t *testing.T) {
	path := tmpPath(t)
	a := notifyOpen(t, path)
	b := notifyOpen(t, path)
	notifyCommit(t, b, "k", "x", "1")
	from, err := a.Version()
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	ch := waitKeyspace(a, "k", from)
	time.Sleep(20 * time.Millisecond)
	tx, err := b.Begin(context.Background())
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := tx.SetKeyspaceConfig("k", KeyspaceConfig{RestartGroupTarget: 8}); err != nil {
		t.Fatalf("SetKeyspaceConfig: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	expectWake(t, ch, "config change")
}

// TestNotifyPublishedOnClassifiedVisibleFailure: a commit whose
// error classifies visible/durability-unknown IS visible to peers
// (verified meta readback), so the notification publish must run on
// those paths too — a waiter must not sleep through a commit it can
// already read. Forced via the landed-meta-write fault (the meta
// pwrite forwards, then reports an error → ErrCommitDurabilityUnknown
// under SyncDurable).
func TestNotifyPublishedOnClassifiedVisibleFailure(t *testing.T) {
	path := tmpPath(t)
	db := commitOutcomeFixture(t, path)
	defer db.Close()
	from, err := db.Version()
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	gch := make(chan error, 1)
	kch := make(chan error, 1)
	go func() {
		_, err := db.WaitVersion(context.Background(), from)
		gch <- err
	}()
	go func() {
		_, err := db.WaitKeyspaceVersion(context.Background(), "k", from)
		kch <- err
	}()
	time.Sleep(20 * time.Millisecond)
	fops := &faultOps{inner: db.WriterFileOpsForTest()}
	fops.failWriteAt = func(off int64) (bool, bool) { return off < 2*4096, true }
	if cerr := commitUnderFault(t, db, fops); !errors.Is(cerr, ErrCommitDurabilityUnknown) {
		t.Fatalf("fixture fault = %v, want ErrCommitDurabilityUnknown", cerr)
	}
	for name, ch := range map[string]chan error{"global": gch, "keyspace": kch} {
		select {
		case err := <-ch:
			if err != nil {
				t.Fatalf("%s waiter: %v", name, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s waiter slept through a classified-visible commit", name)
		}
	}
}

// versionAtMetaWriteOps forwards all I/O and records db.Version() at
// the moment the meta pwrite is intercepted (offsets inside the
// dual-meta pair, the file's first two pages).
type versionAtMetaWriteOps struct {
	inner      pager.FileOps
	db         *DB
	pageSize   int64
	intercepts int
	recorded   []uint64
	verErrs    []error
}

func (v *versionAtMetaWriteOps) WriteAt(p []byte, off int64) (int, error) {
	if off < 2*v.pageSize {
		v.intercepts++
		ver, err := v.db.Version()
		if err != nil {
			v.verErrs = append(v.verErrs, err)
		} else {
			v.recorded = append(v.recorded, ver)
		}
	}
	return v.inner.WriteAt(p, off)
}
func (v *versionAtMetaWriteOps) ReadAt(p []byte, off int64) (int, error) {
	return v.inner.ReadAt(p, off)
}
func (v *versionAtMetaWriteOps) Truncate(size int64) error { return v.inner.Truncate(size) }
func (v *versionAtMetaWriteOps) Fdatasync() error          { return v.inner.Fdatasync() }

// TestNotifyPublishNotBeforeMetaPublication pins the publish-after-
// visibility ordering deterministically (cross-process.md
// §Notification region, publish ordering): at the instant of the
// commit's meta pwrite, the global version word must still be at its
// pre-commit value — the stamp that wakes waiters comes strictly
// after the publication they must be able to observe.
func TestNotifyPublishNotBeforeMetaPublication(t *testing.T) {
	path := tmpPath(t)
	db := commitOutcomeFixture(t, path)
	defer db.Close()
	from, err := db.Version()
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	ops := &versionAtMetaWriteOps{inner: db.WriterFileOpsForTest(), db: db, pageSize: 4096}
	if err := commitUnderFault(t, db, ops); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if ops.intercepts == 0 {
		t.Fatal("fixture failed: no meta pwrite intercepted")
	}
	if len(ops.verErrs) > 0 {
		t.Fatalf("Version errored during %d of %d interceptions: %v", len(ops.verErrs), ops.intercepts, ops.verErrs[0])
	}
	for i, ver := range ops.recorded {
		if ver != from {
			t.Fatalf("notify word advanced to %d (from %d) BEFORE meta pwrite %d — waiters can wake before the commit is publishable", ver, from, i)
		}
	}
	after, err := db.Version()
	if err != nil {
		t.Fatalf("Version after commit: %v", err)
	}
	if after <= from {
		t.Fatalf("commit did not advance the version: %d then %d", from, after)
	}
}

// TestNotifyReadOnlyFallback: on the lock-free read-only fallback
// (no lock file), Version derives from the committed data file and
// WaitVersion degrades to a context-bounded poll.
func TestNotifyReadOnlyFallback(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses file permissions; cannot force a lock-file open failure")
	}
	ctx := context.Background()
	path := tmpPath(t)
	db := notifyOpen(t, path)
	notifyCommit(t, db, "k", "a", "1")
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	lockPath := filepath.Join(filepath.Dir(path), lock.BaseFor(filepath.Base(path)))
	if err := os.Chmod(lockPath, 0o444); err != nil {
		t.Fatalf("chmod lock file: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(lockPath, 0o644) })

	ro, err := Open(ctx, path, Options{ReadOnly: true})
	if err != nil {
		t.Fatalf("read-only Open (fallback): %v", err)
	}
	defer ro.Close()
	if ro.coord != nil {
		t.Fatal("fixture did not hit the lock-free fallback")
	}
	v, err := ro.Version()
	if err != nil {
		t.Fatalf("Version (fallback): %v", err)
	}
	if v == 0 {
		t.Fatal("fallback Version = 0, want the committed TxnID")
	}
	// Already-past from returns immediately; a never-satisfied wait
	// ends with the context.
	got, err := ro.WaitVersion(ctx, v-1)
	if err != nil || got < v {
		t.Fatalf("fallback WaitVersion(past) = (%d, %v), want (>=%d, nil)", got, err, v)
	}
	shortCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	if _, err := ro.WaitVersion(shortCtx, v); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("fallback WaitVersion(future) = %v, want DeadlineExceeded", err)
	}
	// Keyspace waits degrade to global waits in the fallback: an
	// already-past from returns immediately regardless of which
	// keyspace last changed, and a future one blocks on the context.
	got, err = ro.WaitKeyspaceVersion(ctx, "k", v-1)
	if err != nil || got < v {
		t.Fatalf("fallback WaitKeyspaceVersion(past) = (%d, %v), want (>=%d, nil)", got, err, v)
	}
	shortCtx2, cancel2 := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel2()
	if _, err := ro.WaitKeyspaceVersion(shortCtx2, "k", v); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("fallback WaitKeyspaceVersion(future) = %v, want DeadlineExceeded", err)
	}
}

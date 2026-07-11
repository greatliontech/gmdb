package gmdb

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// batchCall is one queued DB.Batch invocation. result is buffered (cap 1)
// so the coordinator never blocks delivering the outcome, even if the
// caller has stopped waiting.
type batchCall struct {
	fn     func(tx *Tx) error
	ctx    context.Context
	result chan error
}

// batchCoordinator holds the lazily-started Batch() coordinator's
// lifecycle state (transactions.md §Write Batching). The goroutine is
// started on the first DB.Batch call (ensureBatchCoordinator) and
// stopped by Close (stopBatchCoordinator); mu guards the start/stop
// lifecycle fields.
type batchCoordinator struct {
	mu      sync.Mutex
	ch      chan batchCall     // calls from Batch → coordinator
	done    chan struct{}      // closed when the coordinator exits
	ctx     context.Context    // coordinator lifetime; its write tx uses it
	cancel  context.CancelFunc // cancels ctx on Close
	started bool               // coordinator goroutine launched
	closed  bool               // Close ran — refuse to (re)start
}

// Batch submits fn to be batched with other concurrent callers into a
// single write transaction (transactions.md §Write Batching). fn runs in
// its own child transaction and is invoked EXACTLY ONCE — there is no
// rollback-and-retry. The context governs the wait for batch inclusion;
// once fn is dequeued the coordinator checks fn's caller context itself.
//
// On success the caller receives nil (after the parent batch commits). If
// fn returns an error, its child is rolled back and the caller receives
// that error; sibling closures are unaffected. If fn panics, the
// coordinator recovers it, rolls the child back, and the caller receives
// ErrBatchClosurePanic wrapping the panic value; siblings still run. A
// closure that exits via runtime.Goexit (t.FailNow and friends) is
// contained the same way and its caller receives ErrBatchClosureGoexit.
// If the parent batch commit fails, every caller whose closure succeeded
// receives the commit error.
//
// Once a call is ACCEPTED into a batch, the caller blocks until the
// batch resolves — past acceptance the ctx can no longer unblock the
// wait. It is consulted once more before dispatch (a fired ctx skips
// the closure — at most once — and the caller receives
// context.Cause); once dispatched, the outcome is reported truthfully
// regardless of ctx (returning early would misreport a write that may
// still land).
//
// The closure MUST NOT call Commit or Rollback on the supplied *Tx — the
// coordinator owns child-transaction lifecycle; doing so makes the
// coordinator's subsequent child commit/rollback error, which propagates
// to the caller. A closure that COMMITS its child anyway gets
// ErrTxClosed back from the coordinator's own child commit — note the
// write STILL LANDS if the parent batch commits (the self-commit
// already merged it): the error reports the contract violation, not
// the write's outcome.
//
// Errors:
//   - context.Cause(ctx) if ctx fires before the call is queued.
//   - ErrClosed if the DB is closing / closed.
func (db *DB) Batch(ctx context.Context, fn func(tx *Tx) error) error {
	if db.readOnly {
		return ErrDatabaseReadOnly
	}
	if err := ctx.Err(); err != nil {
		return context.Cause(ctx)
	}
	if db.closeGate.IsClosed() {
		return ErrClosed
	}
	ch, coordCtx, ok := db.ensureBatchCoordinator()
	if !ok {
		return ErrClosed
	}
	call := batchCall{fn: fn, ctx: ctx, result: make(chan error, 1)}
	select {
	case ch <- call:
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-coordCtx.Done():
		// Coordinator stopping (Close). The call was never accepted.
		return ErrClosed
	}
	// Accepted — the coordinator guarantees exactly one result (it checks
	// call.ctx itself before running fn).
	return <-call.result
}

// ensureBatchCoordinator lazily launches the coordinator goroutine on the
// first Batch call. Returns the call channel, the coordinator-lifetime
// context, and ok=false if Close has already run (refuse to start a
// goroutine that would outlive Close).
func (db *DB) ensureBatchCoordinator() (chan batchCall, context.Context, bool) {
	db.batch.mu.Lock()
	defer db.batch.mu.Unlock()
	if db.batch.closed {
		return nil, nil, false
	}
	if !db.batch.started {
		db.batch.ch = make(chan batchCall)
		db.batch.done = make(chan struct{})
		db.batch.ctx, db.batch.cancel = context.WithCancel(context.Background())
		db.batch.started = true
		go db.batchCoordinator(db.batch.ctx, db.batch.ch, db.batch.done)
	}
	return db.batch.ch, db.batch.ctx, true
}

// stopBatchCoordinator signals the coordinator to stop and waits for it
// to exit. Called by Close before the pager / coord teardown so the
// coordinator's in-flight write transaction (if any) unwinds first and
// no new batch starts afterward. Idempotent and safe if the coordinator
// was never started.
func (db *DB) stopBatchCoordinator() {
	db.batch.mu.Lock()
	db.batch.closed = true
	started := db.batch.started
	cancel := db.batch.cancel
	done := db.batch.done
	db.batch.mu.Unlock()
	if !started {
		return
	}
	cancel()
	<-done
}

// batchCoordinator is the single goroutine that drains db.batch.ch,
// groups calls into batches, and runs each batch in one write
// transaction. It exits when ctx is cancelled (Close).
func (db *DB) batchCoordinator(ctx context.Context, ch chan batchCall, done chan struct{}) {
	defer close(done)
	for {
		select {
		case first := <-ch:
			batch := db.collectBatch(ctx, ch, first)
			db.runBatch(ctx, batch)
		case <-ctx.Done():
			return
		}
	}
}

// collectBatch gathers calls after the first until MaxBatchSize calls
// have accumulated or MaxBatchDelay elapses since the first call (or the
// coordinator is asked to stop). The first call is already in the batch.
func (db *DB) collectBatch(ctx context.Context, ch chan batchCall, first batchCall) []batchCall {
	batch := []batchCall{first}
	maxSize := db.opts.MaxBatchSize
	if len(batch) >= maxSize {
		return batch
	}
	timer := time.NewTimer(db.opts.MaxBatchDelay)
	defer timer.Stop()
	for {
		select {
		case c := <-ch:
			batch = append(batch, c)
			if len(batch) >= maxSize {
				return batch
			}
		case <-timer.C:
			return batch
		case <-ctx.Done():
			return batch
		}
	}
}

// runBatch executes one batch in a single write transaction. Each closure
// runs in its own child transaction (exactly once); per-closure errors,
// panics, and caller-context cancellation are isolated to that caller.
// Successful closures' results are delivered only after the parent batch
// commits — so a commit failure surfaces uniformly to every caller whose
// closure succeeded.
func (db *DB) runBatch(ctx context.Context, batch []batchCall) {
	// Coordinator stopping: the batch was collected but the DB is closing.
	if ctx.Err() != nil {
		replyAll(batch, ErrClosed)
		return
	}
	// The batch transaction uses the coordinator context (caller contexts
	// are checked per-closure below) so a single caller's cancellation
	// never aborts the shared commit, while Close can still unblock the
	// lock wait.
	tx, err := db.Begin(ctx)
	if err != nil {
		// The coordinator ctx is cancelled ONLY by Close; Begin then
		// surfaces context.Canceled, but the caller-facing contract is
		// "ErrClosed if the DB is closing / closed" — map it (the
		// callers never see the coordinator's private context).
		if ctx.Err() != nil && errors.Is(err, context.Canceled) {
			err = ErrClosed
		}
		replyAll(batch, err)
		return
	}
	// Each call's result channel is buffered (cap 1) and written EXACTLY
	// once: a call is replied to in this loop (skip / closure-error /
	// panic / child-commit-error / unresolved-descendant) OR appended to
	// succeeded for a single deferred reply after the parent commit — the
	// two are disjoint by the continue statements below.
	succeeded := make([]batchCall, 0, len(batch))
	for _, c := range batch {
		// Skip a caller whose context fired before its closure ran.
		if err := c.ctx.Err(); err != nil {
			c.result <- context.Cause(c.ctx)
			continue
		}
		child, err := tx.BeginChild()
		if err != nil {
			// Should not happen (parent has no other open child here);
			// surface to this caller and continue with the rest.
			c.result <- err
			continue
		}
		cerr := invokeClosure(child, c.fn)
		if child.activeChild != nil {
			// The closure returned having left a nested child transaction
			// open (it called child.BeginChild without resolving it). An
			// ordinary Rollback is frozen by that descendant, so cascade-
			// resolve to clear the freeze — otherwise the whole batch tx
			// would stay frozen and every sibling + the parent commit
			// would fail (spec §Write Batching clause 5: siblings + parent
			// unaffected by one closure's fault). Fail just this caller.
			child.cascadeRollback()
			if cerr == nil {
				cerr = fmt.Errorf("gmdb: batch closure returned with an unresolved child transaction: %w", ErrChildActive)
			}
			c.result <- cerr
			continue
		}
		if cerr != nil {
			_ = child.Rollback()
			c.result <- cerr
			continue
		}
		if err := child.Commit(); err != nil {
			c.result <- err
			continue
		}
		succeeded = append(succeeded, c)
	}
	if err := tx.Commit(); err != nil {
		// Release the grant + abort pager state if Commit bailed before
		// its own cleanup ran (e.g. ErrClosed on a Close race, which
		// returns ahead of Commit's deferred releaseGrant). On a normal
		// poison/commit failure this is a no-op (tx already closed).
		_ = tx.Rollback()
		replyAll(succeeded, err)
		return
	}
	replyAll(succeeded, nil)
}

// invokeClosure runs fn against the child transaction on a DEDICATED
// goroutine, converting a panic into an error and containing
// runtime.Goexit (e.g. a closure calling t.FailNow) so one misbehaving
// closure cannot crash — or silently unwind — the coordinator or harm
// sibling callers (transactions.md §Write Batching). Goexit runs the
// worker's deferred recover(), which returns nil for Goexit, so the
// completion flag is the discriminator: unset without a recovered
// panic means the closure exited via Goexit. The channel close
// happens-after the flag/err writes (defer LIFO), so the coordinator's
// reads are ordered. The child is rolled back by the caller on any
// non-nil return.
func invokeClosure(child *Tx, fn func(tx *Tx) error) error {
	var (
		err       error
		completed bool
		done      = make(chan struct{})
	)
	go func() {
		defer close(done)
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("%w: %v", ErrBatchClosurePanic, r)
				completed = true
			}
		}()
		err = fn(child)
		completed = true
	}()
	<-done
	if !completed {
		return ErrBatchClosureGoexit
	}
	return err
}

// replyAll delivers err to every call's result channel (buffered, so
// non-blocking).
func replyAll(batch []batchCall, err error) {
	for _, c := range batch {
		c.result <- err
	}
}

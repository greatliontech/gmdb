package lock

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// ErrClosed is returned by AcquireWriter when the Coord is closed
// before the call can be served — either because Close ran before the
// request entered the goroutine, or because Close raced an in-flight
// request and the goroutine exited before granting.
var ErrClosed = errors.New("lock: coord closed")

// ErrReadOnlyCoord is returned by AcquireWriter on a Coord constructed
// with CoordOptions.ReadOnly — a read-only handle never takes LOCK_EX,
// so the flock-grant goroutine is not started and there is nothing to
// grant. Defensive: the gmdb layer rejects writes (ErrDatabaseReadOnly)
// before ever reaching AcquireWriter, so this is a backstop, not a
// reachable production path.
var ErrReadOnlyCoord = errors.New("lock: AcquireWriter on a read-only coord")

// DefaultRetryInterval bounds Close() and per-writer ctx-cancellation
// latency under sustained cross-process contention. The goroutine
// burns one wasted flock(LOCK_EX|LOCK_NB) syscall per tick while
// another process holds the lock; 50 ms keeps Close-latency at one
// tick worst-case while keeping the contended-syscall rate at 20/s.
// Exported so the gmdb Options layer shares a single source of truth
// for the LockRetryInterval default (cross-process.md §Write Lock).
const DefaultRetryInterval = 50 * time.Millisecond

// DefaultHeartbeatInterval matches the cross-process.md §Heartbeat
// Goroutine default ("ticks every ~1 s"). Must remain well under
// StaleTimeout (10 s) so a few missed ticks don't trip false-stale
// detection. Exported so the gmdb Options layer shares a single
// source of truth for the HeartbeatInterval default.
const DefaultHeartbeatInterval = 1 * time.Second

// Coord owns cross-process write-lock acquisition for one *File. A
// single "flock goroutine" runs for the lifetime of the Coord and is
// the only goroutine in the process that ever calls flock() on the
// lock file (cross-process.md §Write Lock invariant). Writers queue
// via AcquireWriter; the goroutine grants one at a time, writes the
// writer-identity fields into the lock-file header under flock(LOCK_EX),
// and clears them before releasing the flock.
//
// Lifecycle:
//   - NewCoord starts the goroutine. The Coord borrows *File — the
//     caller retains ownership and must not Close the *File until
//     after Coord.Close returns.
//   - AcquireWriter is safe to call from any goroutine; concurrent
//     callers are serialised by the channel into the flock goroutine.
//   - Close stops the goroutine. If the lock is held when Close runs,
//     the goroutine clears the header and releases flock before exit.
//     Close blocks until the goroutine has fully exited so the caller
//     can subsequently unmap / close the *File without racing the
//     final unlock.
type Coord struct {
	f         *File
	pid       uint64
	startTime uint64
	pidNS     uint64

	writerCh chan writerRequest
	stopCh   chan struct{}
	doneCh   chan struct{}

	retryInterval time.Duration

	// activeSlotsMu protects activeSlots. cross-process.md §Heartbeat
	// Goroutine: RegisterReaderSlot / UnregisterReaderSlot mutate
	// under the mutex; the heartbeat goroutine snapshots-and-releases
	// before writing slot Heartbeats outside the lock to keep tick
	// cost bounded.
	activeSlotsMu sync.Mutex
	activeSlots   []uint32

	heartbeatInterval time.Duration
	staleTimeout      time.Duration
	clock             func() uint64
	heartbeatDoneCh   chan struct{}

	// readOnly marks a Coord that backs a read-only DB handle
	// (CoordOptions.ReadOnly). The flock-grant goroutine (run) is not
	// started — only the heartbeat goroutine runs, refreshing the
	// reader slots this handle acquires so a concurrent cross-process
	// writer cannot stale-evict them. AcquireWriter returns
	// ErrReadOnlyCoord; Close waits only on the heartbeat goroutine.
	readOnly bool

	// readerSlotHint is the process-local scan-start offset for
	// AcquireReader (cross-process.md §Reader Table: "Start scanning
	// from the slot hint ... rather than slot 0"). atomic.Uint32 to
	// allow concurrent reader-tx Begins to read/CAS without taking a
	// mutex; updates are relaxed stores per spec ("no cross-process
	// coordination") because the hint is a steady-state optimisation,
	// not a correctness mechanism.
	readerSlotHint atomic.Uint32

	// everWriter latches once this handle has held the write grant;
	// the heartbeat goroutine then keeps the lock file's LastWriter
	// record fresh for the handle's lifetime — while it still names
	// this process (format.go LastWriter*, durability.md §Recovery
	// step 5).
	everWriter atomic.Bool

	// writerHeld is true while an in-process caller holds the write
	// grant (set/cleared by the flock goroutine around its hold
	// loop). Consumers: Close's shutdown checkpoint, which must skip
	// rather than deadlock when its own process's live transaction
	// holds the grant (durability.md §Clean shutdown) — waiting on a
	// CROSS-process holder is fine (bounded by the peer's commit
	// window), but an in-process holder cannot release until after
	// Close returns.
	writerHeld atomic.Bool

	// prevLastWriter snapshots the LastWriter record as it stood
	// IMMEDIATELY BEFORE this handle's most recent grant acquisition
	// overwrote it — taken by the flock goroutine under the same
	// LOCK_EX as the overwrite, so it cannot race a peer's write. The
	// recovery-commit gate reads it (via PrevLastWriterLive) because
	// the acquisition itself stamps OUR identity into the live record,
	// destroying the evidence the gate needs. Written only by the
	// flock goroutine; read by the grant holder (happens-after via the
	// grant result channel).
	prevLastWriter struct {
		pid       uint64
		startTime uint64
		pidNS     uint64
		heartbeat uint64
	}

	closeOnce sync.Once
}

// writerRequest is the in-process message from a caller to the flock
// goroutine. The caller allocates all three channels per call so two
// in-flight requests cannot cross-signal each other.
type writerRequest struct {
	ctx     context.Context
	release chan struct{} // closed by Grant.Release; signals step-4 release
	result  chan error    // single value: nil = grant, non-nil = denied
}

// Grant is returned by AcquireWriter on success. Release MUST be
// called exactly once per Grant when the writer is done — failing to
// call Release leaves the flock held until Coord.Close.
//
// Release is safe to call from any goroutine and is idempotent
// (sync.Once-guarded). Calling Release after Coord.Close runs
// `close(g.release)` against a no-longer-listening goroutine; the
// close itself is non-panicking because the goroutine never closes
// `release` — only the caller side does, via Grant.Release when a
// Grant escapes or via AcquireWriter's ctx-cancel drain path when
// it does not. Two caller-side close sites both flow through
// sync.Once-guarded code (Grant.once for Release; the drain path
// is single-shot per call), so no two closes ever race. A future
// re-design must preserve "no goroutine-side close" — that is the
// invariant that makes Release-after-Close non-panicking. The
// goroutine's stopCh path is what actually releases the kernel
// flock during Close.
type Grant struct {
	release chan<- struct{}
	once    sync.Once
	// coord backs the synchronous writerHeld clear in Release; see
	// that method. Nil only in zero-value Grants (never issued).
	coord *Coord
}

// Release signals the flock goroutine to clear the writer-header
// fields and release flock(LOCK_UN). Idempotent.
//
// writerHeld is cleared HERE, synchronously, not in the flock
// goroutine's hold-loop exit: Release returns before the goroutine
// processes the channel close, and WriterHeld's consumer (Close's
// shutdown checkpoint) runs immediately after a released grant — a
// stale true would silently skip the shutdown checkpoint. The
// goroutine's own exit paths (stopCh) clear it too, idempotently.
func (g *Grant) Release() {
	if g == nil {
		return
	}
	g.once.Do(func() {
		if g.coord != nil {
			g.coord.writerHeld.Store(false)
		}
		close(g.release)
	})
}

// CoordOptions configures NewCoord. The PID/ProcessStartTime/
// PIDNamespace are the caller's *cached* identity values, computed
// once at db.Open and passed in here — see cross-process.md §Process
// Start Time and §PID Namespace Awareness for the caching rationale.
// The Coord does not re-derive them per grant.
type CoordOptions struct {
	// PID is the value written into the lock-file's WriterPID field
	// on grant. Typically uint64(os.Getpid()). Zero is allowed but
	// causes peer-process stale detection to route through the
	// heartbeat path (see cross-process.md §Stale writer recovery,
	// case 2).
	PID uint64

	// ProcessStartTime is the value written into WriterStartTime.
	// Sourced via lock.ProcessStartTime(os.Getpid()); on error the
	// caller passes 0 and peer stale-detection falls back to the
	// heartbeat path.
	ProcessStartTime uint64

	// PIDNamespace is the value written into WriterPIDNamespace.
	// Sourced via lock.PIDNamespace(); 0 on non-Linux or hardened-
	// sandbox /proc-unavailable hosts.
	PIDNamespace uint64

	// RetryInterval is the flock(LOCK_EX|LOCK_NB) retry tick. Zero ⇒
	// DefaultRetryInterval (50 ms). Bounds Close() and per-writer
	// ctx-cancellation latency under sustained contention.
	RetryInterval time.Duration

	// HeartbeatInterval is the refresh cadence for both heartbeat
	// fields: the flock goroutine refreshes WriterHeartbeat at this
	// interval while it holds LOCK_EX (step-4 hold loop), and the
	// heartbeat goroutine refreshes the Heartbeat field of every
	// reader slot in the active list. Zero ⇒ DefaultHeartbeatInterval
	// (1 s). Must remain well under StaleTimeout (10 s, per
	// cross-process.md §Heartbeat Goroutine).
	HeartbeatInterval time.Duration

	// StaleTimeout is how long a reader-slot or writer Heartbeat must
	// be older than the scan clock before stale-detection reclaims it
	// (cross-process.md §Heartbeat Goroutine). Zero ⇒
	// DefaultStaleTimeout (10 s). Consumed by OldestReaderTxnID (and
	// thus ReapStaleReaderSlots); the writer-recovery path under
	// LOCK_EX is timeout-independent (any non-zero WriterPID held while
	// we own LOCK_EX is definitionally stale). Must be significantly
	// larger than HeartbeatInterval for scheduling jitter — the caller
	// (gmdb.Options.validate) enforces StaleTimeout > HeartbeatInterval.
	StaleTimeout time.Duration

	// Clock returns the monotonic clock value in nanoseconds. Nil ⇒
	// the per-platform default (CLOCK_BOOTTIME on Linux,
	// CLOCK_MONOTONIC elsewhere). Tests inject deterministic clocks
	// here to assert heartbeat values without flakiness.
	Clock func() uint64

	// ReadOnly marks a Coord backing a read-only DB handle. The
	// flock-grant goroutine is not started (the handle never takes
	// LOCK_EX); AcquireWriter returns ErrReadOnlyCoord. The heartbeat
	// goroutine and the reader-slot path (AcquireReader / ReleaseReader)
	// run unchanged so read transactions still pin their snapshots
	// against a concurrent cross-process writer's reclamation.
	ReadOnly bool
}

// NewCoord constructs a Coord and starts its flock goroutine. The
// goroutine runs until Close is called. The caller retains ownership
// of f — Close on the Coord does not close the underlying *File.
func NewCoord(f *File, opts CoordOptions) *Coord {
	retry := opts.RetryInterval
	if retry <= 0 {
		retry = DefaultRetryInterval
	}
	hbInterval := opts.HeartbeatInterval
	if hbInterval <= 0 {
		hbInterval = DefaultHeartbeatInterval
	}
	staleTimeout := opts.StaleTimeout
	if staleTimeout <= 0 {
		staleTimeout = DefaultStaleTimeout
	}
	clock := opts.Clock
	if clock == nil {
		clock = nowMonotonic
	}
	c := &Coord{
		f:                 f,
		pid:               opts.PID,
		startTime:         opts.ProcessStartTime,
		pidNS:             opts.PIDNamespace,
		writerCh:          make(chan writerRequest),
		stopCh:            make(chan struct{}),
		doneCh:            make(chan struct{}),
		retryInterval:     retry,
		heartbeatInterval: hbInterval,
		staleTimeout:      staleTimeout,
		clock:             clock,
		heartbeatDoneCh:   make(chan struct{}),
		readOnly:          opts.ReadOnly,
	}
	// A read-only coord never takes LOCK_EX, so the flock-grant
	// goroutine is not started. Close must not wait on doneCh in that
	// case — nothing closes it. The heartbeat goroutine still runs to
	// keep this handle's reader slots live.
	if !c.readOnly {
		go c.run()
	} else {
		close(c.doneCh)
	}
	go c.heartbeat()
	return c
}

// Close stops the flock + heartbeat goroutines and waits for both to
// exit. If a writer holds the lock at Close time, the flock goroutine
// clears the writer-header fields and releases flock before
// returning — so the next opener does not see a (PID set, flock free)
// inconsistency (cross-process.md clear-before-unlock + Close-
// releases invariants).
//
// The heartbeat goroutine is also drained before Close returns
// (cross-process.md §Heartbeat Goroutine shutdown coordination): a
// final tick must not race the *File's munmap. Callers must complete
// Coord.Close before Closing the underlying *File.
//
// Idempotent.
func (c *Coord) Close() error {
	c.closeOnce.Do(func() {
		close(c.stopCh)
		<-c.doneCh
		<-c.heartbeatDoneCh
	})
	return nil
}

// AcquireWriter blocks until the flock goroutine grants the write
// lock, ctx fires, or the Coord is closed. On success the returned
// *Grant.Release MUST be called when the writer is done.
//
// Cancellation semantics. If ctx fires after the request has been
// queued, AcquireWriter does not leak a held flock: it drains the
// result channel to discover whether the goroutine had already
// granted, and closes the release channel in that case so the
// goroutine immediately clears + unlocks.
//
// On ErrClosed, no flock is held and no header fields were written
// against this caller — Close ordering guarantees that a request
// observed as denied via stopCh was never granted.
func (c *Coord) AcquireWriter(ctx context.Context) (*Grant, error) {
	if c.readOnly {
		// Defensive backstop: the flock-grant goroutine was never
		// started, so there is no receiver on writerCh — a send would
		// block forever. The gmdb layer rejects writes with
		// ErrDatabaseReadOnly long before here.
		return nil, ErrReadOnlyCoord
	}
	if err := ctx.Err(); err != nil {
		return nil, context.Cause(ctx)
	}

	release := make(chan struct{})
	result := make(chan error, 1)
	req := writerRequest{ctx: ctx, release: release, result: result}

	select {
	case c.writerCh <- req:
	case <-ctx.Done():
		return nil, context.Cause(ctx)
	case <-c.stopCh:
		return nil, ErrClosed
	}

	select {
	case err := <-result:
		if err != nil {
			return nil, err
		}
		return &Grant{release: release, coord: c}, nil
	case <-ctx.Done():
		// ctx fired after submit. The goroutine may have already
		// granted (and is now in step 4 holding flock). Drain to find
		// out — and release on our behalf if so. The select on stopCh
		// covers the (rare) case where the goroutine shut down before
		// processing our request: stopCh ensures we don't block
		// forever on result.
		select {
		case err := <-result:
			if err == nil {
				close(release)
			}
		case <-c.stopCh:
			// Goroutine already exited; if it had granted us, its
			// stopCh path cleared the header and released flock —
			// nothing left for us to do.
		}
		return nil, context.Cause(ctx)
	case <-c.stopCh:
		// Goroutine exited before granting. No flock held; no header
		// fields written. The request itself sits abandoned in the
		// goroutine-receive side of writerCh (which is unbuffered, so
		// either the goroutine did receive it before exiting — in
		// which case it ran through to a result write or to its stopCh
		// branch — or our send case wouldn't have selected). Either
		// way, no held resources.
		return nil, ErrClosed
	}
}

// run is the flock goroutine's main loop. It is the only goroutine in
// the process permitted to call flock() on c.f.
func (c *Coord) run() {
	defer close(c.doneCh)
	ticker := time.NewTicker(c.retryInterval)
	defer ticker.Stop()

	for {
		select {
		case <-c.stopCh:
			return
		case req := <-c.writerCh:
			if c.process(req, ticker.C) {
				return
			}
		}
	}
}

// process handles one writer request end-to-end. Returns true iff
// stopCh fired during processing — in that case any held flock has
// already been released and the goroutine should exit.
//
// Send-on-result discipline: exactly one value is sent to req.result
// per call, except on the stopCh-during-step-4 branch where the result
// was sent at grant time (step 3) and the stopCh return signals the
// goroutine to exit after releasing flock. The acquireLoop-stopCh
// branch returns true *without* a result send (caller's outer
// AcquireWriter select handles stopCh directly).
func (c *Coord) process(req writerRequest, tick <-chan time.Time) bool {
	// Step 2a: pre-flock cancellation check. An optimisation, not a
	// correctness guarantee — the ctx can race during step 2b's
	// EWOULDBLOCK loop, which has its own ctx select.
	if err := req.ctx.Err(); err != nil {
		req.result <- context.Cause(req.ctx)
		return false
	}

	// Step 2b: non-blocking acquisition with retry. cross-process.md
	// §Write Lock invariant: never blocking flock(LOCK_EX) — the
	// LOCK_NB variant lets us interleave the ticker / ctxDone / stopCh
	// select between retries.
	//
	// EINTR handling: LOCK_NB shouldn't sleep, but the Go runtime's
	// SIGURG-based preemption can interrupt the syscall and surface
	// EINTR. Treat it like "no decision yet" — retry immediately
	// without consuming a tick (the kernel didn't determine
	// contention; we should not stall a writer on a signal artifact).
	// The pre-retry non-blocking select keeps stopCh/ctx responsive
	// even under a hypothetical sustained EINTR stream — a fully-
	// preempting kernel still cannot starve shutdown.
	for {
		err := syscall.Flock(int(c.f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if errors.Is(err, syscall.EINTR) {
			select {
			case <-c.stopCh:
				return true
			case <-req.ctx.Done():
				req.result <- context.Cause(req.ctx)
				return false
			default:
				continue
			}
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			req.result <- err
			return false
		}
		select {
		case <-tick:
			continue
		case <-req.ctx.Done():
			req.result <- context.Cause(req.ctx)
			return false
		case <-c.stopCh:
			return true
		}
	}

	// Step 2c: stale-writer recovery. Now that we hold LOCK_EX, any
	// non-zero WriterPID in the header MUST be stale — the clear-
	// before-unlock invariant (cross-process.md §Invariants)
	// guarantees a clean releaser stores PID = 0 before LOCK_UN, so
	// a non-zero PID we observe here is from a process that crashed
	// or was killed without reaching its step-4 cleanup. RecoverStale-
	// Writer clears the header and (if same-namespace) cleans up any
	// reader slots owned by the dead writer. This is the only place
	// in the lock package that ever issues a same-OFD reader-slot
	// clear; activating it before the publish ensures step-3's
	// header values aren't immediately overwritten by recovery.
	if c.f.WriterPID() != 0 {
		RecoverStaleWriter(c.f, c.pidNS)
	}

	// Step 3: publish writer identity. Order among these three is NOT
	// load-bearing — same-namespace stale-writer detection inspects
	// all three jointly under cross-process.md §Stale Writer Recovery,
	// unlike reader-slot acquire (cross-process.md §Reader Table)
	// whose Heartbeat→HintEpoch→PIDNamespace→ProcessStartTime→PID
	// ordering IS load-bearing. The flock-as-mutex is what makes the
	// asymmetry safe: no peer can observe these fields without first
	// taking LOCK_SH or LOCK_EX, both of which we exclude via our held
	// LOCK_EX. The publish-before-flock-release ordering is what's
	// load-bearing (the clear-before-unlock invariant, step 4).
	//
	// WriterHeartbeat: published synchronously under LOCK_EX so any
	// peer observing WriterPID != 0 immediately also sees a non-zero
	// recent heartbeat. Step 4's hold loop refreshes it each
	// HeartbeatInterval tick while we hold LOCK_EX; without this
	// initial publication, a cross-namespace stale-detection scan
	// between grant and the first refresh would see WriterHeartbeat =
	// 0 and false-stale this fresh writer.
	c.f.SetWriterPID(c.pid)
	c.f.SetWriterStartTime(c.startTime)
	c.f.SetWriterPIDNamespace(c.pidNS)
	c.f.SetWriterHeartbeat(c.clock())
	// LastWriter*: the persisted author identity (format.go). Written
	// under the same LOCK_EX, NOT cleared at release — the
	// recovery-commit gate consults it to distinguish a crashed
	// database from a live one with an idle author (durability.md
	// §Recovery step 5). The pre-overwrite record is snapshotted
	// first (same LOCK_EX — race-free) because this very write stamps
	// OUR identity, and the gate must classify the PREVIOUS author.
	// The heartbeat goroutine refreshes LastWriterHeartbeat for this
	// handle's lifetime once set (see heartbeat).
	c.prevLastWriter.pid = c.f.LastWriterPID()
	c.prevLastWriter.startTime = c.f.LastWriterStartTime()
	c.prevLastWriter.pidNS = c.f.LastWriterPIDNamespace()
	c.prevLastWriter.heartbeat = c.f.LastWriterHeartbeat()
	c.f.SetLastWriterPID(c.pid)
	c.f.SetLastWriterStartTime(c.startTime)
	c.f.SetLastWriterPIDNamespace(c.pidNS)
	c.f.SetLastWriterHeartbeat(c.clock())
	c.everWriter.Store(true)
	c.writerHeld.Store(true)
	req.result <- nil

	// Step 4: hold until release or stopCh, refreshing WriterHeartbeat
	// each HeartbeatInterval tick. The flock goroutine holds LOCK_EX
	// continuously across this loop — from the step-3 publish above to
	// the clear+unlock below — so every refresh here lands under
	// LOCK_EX by construction. This is what makes the "WriterHeartbeat
	// is written only while holding LOCK_EX" invariant
	// (cross-process.md §Invariants) hold structurally: this goroutine
	// is the sole writer of WriterHeartbeat, and it writes only inside
	// this hold window. There is no in-process holding flag a separate
	// goroutine could read stale and so stomp the field after our
	// LOCK_UN.
	hbTicker := time.NewTicker(c.heartbeatInterval)
	stopped := false
holdLoop:
	for {
		select {
		case <-req.release:
			break holdLoop
		case <-c.stopCh:
			stopped = true
			break holdLoop
		case <-hbTicker.C:
			c.f.SetWriterHeartbeat(c.clock())
		}
	}
	hbTicker.Stop()
	c.writerHeld.Store(false)

	// Clear header BEFORE unlock — clause-explicit invariant
	// (cross-process.md §Invariants): a peer that acquires LOCK_EX
	// immediately after our LOCK_UN must NOT observe stale
	// WriterPID. Clear-then-unlock preserves that even under rapid
	// LOCK_EX hand-off across processes. WriterHeartbeat is NOT
	// cleared on normal release — stale-detection only consults it
	// when WriterPID != 0, so leaving the last-known value avoids a
	// redundant atomic store. (Recovery-side clearing differs and
	// does clear all four fields; see RecoverStaleWriter.)
	c.f.SetWriterPID(0)
	c.f.SetWriterStartTime(0)
	c.f.SetWriterPIDNamespace(0)

	// Release test hook fires AFTER the header clear and BEFORE the
	// LOCK_UN syscall — i.e., inside the clear-before-unlock window.
	// Production paths leave the pointer nil. Tests use
	// SetReleaseHookForTest to install a witness pause so a peer
	// goroutine can verify (a) the header is already cleared and
	// (b) the flock is still held — directly enforcing the
	// clear-before-unlock invariant. See recovery_test.go's
	// TestClearBeforeUnlockOrdering.
	if hook := releaseHookForTest.Load(); hook != nil {
		(*hook)()
	}

	_ = syscall.Flock(int(c.f.Fd()), syscall.LOCK_UN)
	return stopped
}

// releaseHookForTest is the post-clear / pre-unlock injection point.
// Production paths leave the pointer nil. atomic.Pointer storage so
// concurrent SetReleaseHookForTest does not race the goroutine's
// Load. cf. lock.createInitHookForTest.
var releaseHookForTest atomic.Pointer[func()]

// SetReleaseHookForTest installs (or clears with nil) the post-clear
// / pre-unlock test injection point in the flock-goroutine's release
// path. Tests must restore the prior value via t.Cleanup. See
// recovery_test.go's TestClearBeforeUnlockOrdering for usage.
func SetReleaseHookForTest(hook func()) {
	if hook == nil {
		releaseHookForTest.Store(nil)
		return
	}
	releaseHookForTest.Store(&hook)
}

// heartbeat is the periodic-refresh goroutine started by NewCoord
// and stopped by Close. Per cross-process.md §Heartbeat Goroutine it
// refreshes every active reader slot's Heartbeat field once per
// HeartbeatInterval. It does NOT touch WriterHeartbeat: that field is
// refreshed exclusively by the flock goroutine inside its step-4 hold
// loop (process) — the only context in which this process holds
// LOCK_EX — so the "WriterHeartbeat written only under LOCK_EX"
// invariant holds by construction rather than via a cross-goroutine
// holding flag. The activeSlotsMu snapshot pattern keeps tick cost
// bounded — the lock is held only long enough to copy the slot index
// list; atomic stores happen outside the lock so a slow
// Register/Unregister cannot stall the tick.
func (c *Coord) heartbeat() {
	defer close(c.heartbeatDoneCh)
	ticker := time.NewTicker(c.heartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-c.stopCh:
			return
		case <-ticker.C:
			now := c.clock()
			c.activeSlotsMu.Lock()
			// Snapshot to a local. activeSlots is typically O(active
			// readers) — small. The alloc is the only per-tick
			// allocation; acceptable given a 1 s default cadence.
			slots := append([]uint32(nil), c.activeSlots...)
			c.activeSlotsMu.Unlock()
			for _, idx := range slots {
				slot := c.f.Slot(idx)
				Store64(&slot.Heartbeat, now)
			}
			// Keep the last-writer record fresh for this handle's
			// LIFETIME once it has ever held the grant — but only
			// while the record still names US (a later writer in
			// another process overwrites it; refreshing then would
			// falsify the new author's liveness signal). The
			// check-then-set can race a peer's re-stamp by one tick;
			// the stray refresh freshens the PEER's record —
			// conservative (recovery deferred ≤ one interval), never
			// a false-stale.
			if c.everWriter.Load() && c.f.LastWriterPID() == c.pid && c.f.LastWriterPIDNamespace() == c.pidNS {
				c.f.SetLastWriterHeartbeat(now)
			}
		}
	}
}

// RegisterReaderSlot adds slot index i to the heartbeat goroutine's
// active list. Callers (the read-tx Begin path) invoke this AFTER
// successful slot acquisition. The heartbeat goroutine refreshes
// the slot's Heartbeat field on each subsequent tick until
// UnregisterReaderSlot is called.
//
// No deduplication: registering the same index twice will produce
// two heartbeat writes per tick (harmless — both stores write the
// same value) but UnregisterReaderSlot removes only the first match.
// Internal callers are trusted not to double-register.
func (c *Coord) RegisterReaderSlot(i uint32) {
	c.activeSlotsMu.Lock()
	c.activeSlots = append(c.activeSlots, i)
	c.activeSlotsMu.Unlock()
}

// ActiveReaderSlots reports how many reader slots this handle (this
// process's DB) currently holds — the in-process active read-transaction
// count. Used by Compact to drain in-process readers before swapping the
// file (cross-process readers occupy slots registered by *other* Coords
// and are not counted here). The slot is registered on AcquireReader and
// unregistered on ReleaseReader — whether via explicit Commit/Rollback or
// the leak-detection cleanup — so this tracks live in-process readers
// exactly.
func (c *Coord) ActiveReaderSlots() int {
	c.activeSlotsMu.Lock()
	defer c.activeSlotsMu.Unlock()
	return len(c.activeSlots)
}

// Clock returns the coord's monotonic clock value in nanoseconds (the
// shared CLOCK_BOOTTIME on Linux, or the test-injected clock). The
// background-maintenance goroutine uses it both to compare against and to
// stamp the lock file's LastMaintenanceTime, so the comparison and the
// stamp share one clock source.
func (c *Coord) Clock() uint64 { return c.clock() }

// DataGeneration reads the lock header's data-file replacement
// counter (format.go DataGeneration).
func (c *Coord) DataGeneration() uint64 { return c.f.DataGeneration() }

// BumpDataGeneration increments the counter — Compact's post-rename
// publish step; caller must hold the write grant.
func (c *Coord) BumpDataGeneration() uint64 { return c.f.BumpDataGeneration() }

// StaleTimeout returns the effective per-process stale-detection
// window — the configured CoordOptions.StaleTimeout, or
// DefaultStaleTimeout (10 s) when unset. It governs how long a reader
// slot's (or writer's) Heartbeat may lag the scan clock before
// stale-detection reclaims it; OldestReaderTxnID uses it via
// staleTimeoutNanos. Exposed for diagnostics and to let the gmdb
// Options layer confirm the value it passed in took effect.
func (c *Coord) StaleTimeout() time.Duration { return c.staleTimeout }

// HeartbeatInterval returns the effective heartbeat-refresh cadence —
// the configured CoordOptions.HeartbeatInterval, or
// DefaultHeartbeatInterval (1 s) when unset. Companion to
// StaleTimeout() for diagnostics and Options pass-through assertions.
func (c *Coord) HeartbeatInterval() time.Duration { return c.heartbeatInterval }

// RetryInterval returns the effective flock(LOCK_EX|LOCK_NB) retry
// tick — the configured CoordOptions.RetryInterval, or
// DefaultRetryInterval (50 ms) when unset. Companion to
// StaleTimeout() for diagnostics and Options pass-through assertions.
func (c *Coord) RetryInterval() time.Duration { return c.retryInterval }

// UnregisterReaderSlot removes slot index i from the active list.
// MUST be called BEFORE the reader-side clears the slot's
// PID/Heartbeat/etc. on release (cross-process.md §Heartbeat
// Goroutine "Race note"): otherwise the next heartbeat tick can
// stomp a freshly-reacquired slot with our process's clock value.
//
// Idempotent — removing an absent index is a no-op (no error).
func (c *Coord) UnregisterReaderSlot(i uint32) {
	c.activeSlotsMu.Lock()
	for j, s := range c.activeSlots {
		if s == i {
			// Swap-with-last + truncate. Order doesn't matter; the
			// heartbeat snapshot walks the whole slice per tick.
			last := len(c.activeSlots) - 1
			c.activeSlots[j] = c.activeSlots[last]
			c.activeSlots = c.activeSlots[:last]
			break
		}
	}
	c.activeSlotsMu.Unlock()
}

// WriterHeld reports whether an in-process caller currently holds the
// write grant. See the writerHeld field doc for the intended consumer
// and its deadlock rationale; this is advisory (the state can change
// the instant it is read) and must not be used as a lock.
func (c *Coord) WriterHeld() bool {
	return c.writerHeld.Load()
}

// PrevLastWriterLive reports whether the last-writer record AS IT
// STOOD BEFORE this handle's most recent grant acquisition named a
// live process (durability.md §Recovery step 5). The caller MUST hold
// the grant that acquisition produced (the snapshot is published
// happens-before the grant result). The live record itself is useless
// to the gate — the acquisition stamped this handle's own identity
// into it.
//
// A previous author that is THIS process (a same-process re-open after
// Close) classifies live iff the process is, i.e. always — correct: an
// in-process reopen is a live join, never crash recovery.
func (c *Coord) PrevLastWriterLive() bool {
	p := c.prevLastWriter
	if p.pid == 0 {
		return false
	}
	now := c.clock()
	timeout := c.staleTimeoutNanos()
	sameNS := p.pidNS != 0 && c.pidNS != 0 && p.pidNS == c.pidNS
	if sameNS {
		if !IsAlive(int(p.pid)) {
			return false
		}
		actualStart, err := ProcessStartTime(int(p.pid))
		if err != nil {
			return now-p.heartbeat <= timeout
		}
		return actualStart == p.startTime
	}
	return now-p.heartbeat <= timeout
}

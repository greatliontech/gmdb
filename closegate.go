package gmdb

import (
	"runtime"
	"sync/atomic"
)

// closeGate is the heap-allocated coordination structure shared by
// pointer between the *DB, every txCleanupInfo, every
// readTxCleanupInfo, and the dbCleanupInfo. It composes two
// previously-separate concerns:
//
//  1. The `closed` flag from leak-detection.md §Cleanup Behavior
//     clause-explicit invariant: a release-stored `bool` that any
//     subsequent Tx cleanup loads to decide whether to skip its
//     resource-touching work. Heap allocation + sharing by pointer
//     is required because runtime.AddCleanup provides no ordering
//     guarantee between a DB cleanup and the Tx cleanups that
//     depend on observing it.
//
//  2. The `txInflight` counter — a promotion of the
//     same spec clause to address a race the writer-only earlier
//     promotion didn't surface: a leaked-Tx cleanup that loaded
//     `closed=false` MAY then proceed to touch the lock-file mmap
//     (the read-tx slot release path) while `Close()` advances
//     past its release-store and into the unmap. The `closed`
//     load is sufficient for the writer path (cleanup never
//     touches the lock-file mmap — only the pager's slab and the
//     flock-grant channel, both of which survive `Close()`); it
//     is NOT sufficient for the reader path. The refcount drains
//     in `Close()` before `lockFile.Close()` so cleanups that
//     passed the gate complete before unmap.
//
// Pattern, per cleanup:
//
//	cleanup:
//	  if held.CAS(true→false) {
//	      gate.txInflight.Add(+1)
//	      if gate.closed.Load() {
//	          gate.txInflight.Add(-1); return  // skip work
//	      }
//	      // ... touch lock-file mmap, pager state, etc. ...
//	      gate.txInflight.Add(-1)
//	  }
//
//	DB.Close:
//	  gate.closed.Store(true)            // release-store
//	  for gate.txInflight.Load() > 0 {   // drain
//	      runtime.Gosched()
//	  }
//	  coord.Close(); lockFile.Close(); ... // safe to tear down
//
// Sequential consistency on Go's `sync/atomic` primitives on
// amd64/arm64 (gmdb's supported architectures, per
// cross-process.md §Atomic Operations Convention) gives a total
// order across the closed-Store, closed-Load, and inflight-Add
// events. The interleaving analysis:
//
//   - cleanup Add(+1) before Close Store(true): Close's
//     Load(inflight) sees ≥1, spins until cleanup Add(-1) lands.
//     Cleanup's Load(closed) may see false (Close hasn't stored
//     yet at cleanup-load time, since cleanup-Add < cleanup-Load <
//     Close-Store is one valid order). Cleanup proceeds with its
//     work — but Close is waiting, so no concurrent unmap.
//
//   - Close Store(true) before cleanup Add(+1): cleanup's
//     Load(closed) returns true; cleanup skips work, Add(-1),
//     return. Close's Load(inflight) may briefly see 1, spin, see
//     0, proceed.
//
//   - Concurrent: a brief spin in Close until the racing
//     cleanup's Add(-1) lands. Bounded by cleanup work duration
//     (microseconds for atomic stores on the reader slot).
//
// The wait in Close uses `runtime.Gosched()` rather than
// `time.Sleep` to keep the latency bounded by the scheduler's
// time-slice — cleanups complete in microseconds in practice, so
// a true sleep would over-pessimise.
type closeGate struct {
	closed     atomic.Bool
	txInflight atomic.Int32
}

// newCloseGate returns a freshly-allocated *closeGate with
// closed=false and txInflight=0.
func newCloseGate() *closeGate { return &closeGate{} }

// EnterCleanup is the cleanup-side entry. Returns true if the
// cleanup must perform its full release path; false if the cleanup
// must skip (Close has begun, or the cleanup re-entered after a
// Close). The caller MUST call ExitCleanup exactly once per
// EnterCleanup, regardless of return value — the counter is
// already incremented inside EnterCleanup.
//
// Returning false (skip) is the closed-observed path; the caller
// still logs the leak warning before returning.
func (g *closeGate) EnterCleanup() bool {
	g.txInflight.Add(1)
	if g.closed.Load() {
		return false
	}
	return true
}

// ExitCleanup decrements the in-flight counter. MUST be paired
// 1:1 with EnterCleanup. Safe to call after EnterCleanup returned
// false (the spec gate still requires the matching decrement so
// Close's drain doesn't see a phantom).
func (g *closeGate) ExitCleanup() { g.txInflight.Add(-1) }

// BeginClose stores closed=true (release-store), then spins on
// txInflight until it reaches zero. The spin uses runtime.Gosched
// rather than time.Sleep — cleanups are short atomic-store
// sequences, so a time.Sleep would over-pessimise the common case.
//
// MUST be called exactly once per DB.Close — the inner CAS that
// guards Close's body (db.closeGate.closed.CompareAndSwap) is the
// "exactly once" arbiter; this method assumes the caller has won
// that CAS.
//
// After BeginClose returns, no leaked-Tx cleanup will touch
// lock-file mmap memory (Tx cleanups either skipped via the
// closed-observed path or completed before BeginClose returned).
// The caller may now safely Close the coord, unmap the lock file,
// and close the pager.
//
// Cleanups that fire AFTER BeginClose returns observe closed=true
// (release-store visible by load-acquire happens-before) and skip
// the resource-touching path inside their EnterCleanup gate.
func (g *closeGate) BeginClose() {
	g.closed.Store(true)
	for g.txInflight.Load() > 0 {
		runtime.Gosched()
	}
}

// IsClosed reports whether Close has begun. Used by the Tx-method
// use-after-Close defense (tx.requireOpen / rtx.Page).
func (g *closeGate) IsClosed() bool { return g.closed.Load() }

// CompareAndSwapClosed mirrors atomic.Bool.CompareAndSwap on the
// closed flag. Used by DB.Close's idempotency check (only one
// caller wins, runs BeginClose + drain).
func (g *closeGate) CompareAndSwapClosed(old, new bool) bool {
	return g.closed.CompareAndSwap(old, new)
}

// SwapClosed mirrors atomic.Bool.Swap on the closed flag. Used by
// the DB-cleanup callback as the Close-vs-cleanup gate (whoever
// wins runs the drain; the loser skips).
func (g *closeGate) SwapClosed(new bool) bool { return g.closed.Swap(new) }

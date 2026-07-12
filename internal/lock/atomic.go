package lock

import "sync/atomic"

// Shared-memory atomic helpers. These wrap the function-based
// sync/atomic operations per the cross-process.md §Atomic Operations
// Convention: typed atomics (atomic.Uint64) cannot be used on
// MAP_SHARED memory because the runtime owns their identity and the
// memory model only formalises synchronisation on Go-runtime-owned
// memory.
//
// All callers pass &header.Field or &slot.Field — both addresses are
// 8-byte aligned by construction (HostLayout + page-aligned mmap
// base), which is the precondition sync/atomic's documentation
// requires for 64-bit operations.

// Load64 atomically loads a uint64 from a shared-memory field.
func Load64(p *uint64) uint64 {
	return atomic.LoadUint64(p)
}

// Store64 atomically writes a uint64 to a shared-memory field.
func Store64(p *uint64, v uint64) {
	atomic.StoreUint64(p, v)
}

// CAS64 atomically compares-and-swaps a uint64 in a shared-memory
// field. Returns true if the swap succeeded.
func CAS64(p *uint64, old, new uint64) bool {
	return atomic.CompareAndSwapUint64(p, old, new)
}

// Load32 / Store32 cover the 32-bit shared-memory fields —
// MaxReaders (immutable after creation; the load lets consumers
// sanity-check the header against the size argument used to mmap)
// and TakeoverSeq.
func Load32(p *uint32) uint32     { return atomic.LoadUint32(p) }
func Store32(p *uint32, v uint32) { atomic.StoreUint32(p, v) }

// Add32 atomically adds delta to a shared-memory uint32 field and
// returns the new value — the TakeoverSeq bump.
func Add32(p *uint32, delta uint32) uint32 { return atomic.AddUint32(p, delta) }

// Add64 atomically adds delta to a shared-memory uint64 field and
// returns the new value.
func Add64(p *uint64, delta uint64) uint64 {
	return atomic.AddUint64(p, delta)
}

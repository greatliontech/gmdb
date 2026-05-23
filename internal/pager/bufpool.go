package pager

import "sync"

// BufPool is a process-wide pool of page-sized byte buffers. Get returns a
// zero-filled buffer (either freshly allocated or one previously released
// via Put — released buffers are cleared before re-entering the pool, so
// any Get is safe to use immediately).
//
// The pool is held on the DB (one per database handle, but typically
// shared process-wide via a package-level reference); each write
// transaction acquires buffers from it and returns them at Commit /
// Rollback.
//
// Per pager-slab.md §Slab Budget and `ErrTxTooLarge`: a buffer becoming
// loose mid-transaction is NOT returned to the pool — it is held by the
// pager until tx close to honour the byte-slice ownership contract. Put
// is therefore only called at Commit / Rollback by the pager's release
// path, not at the moment a buffer becomes loose.
type BufPool struct {
	pool     sync.Pool
	pageSize int
}

// NewBufPool constructs a pool whose buffers are exactly pageSize bytes.
func NewBufPool(pageSize int) *BufPool {
	p := &BufPool{pageSize: pageSize}
	p.pool.New = func() any {
		b := make([]byte, pageSize)
		return &b
	}
	return p
}

// PageSize returns the configured buffer size.
func (p *BufPool) PageSize() int { return p.pageSize }

// Get returns a page-sized buffer. The returned buffer is zero-filled (a
// fresh allocation is naturally zeroed; a recycled buffer was cleared on
// Put).
func (p *BufPool) Get() *[]byte {
	return p.pool.Get().(*[]byte)
}

// Put returns a buffer to the pool. Clears the buffer first to satisfy
// the pager-slab.md guarantee that recycled buffers carry no residual
// content. Buffers whose underlying capacity does not match p.pageSize
// are dropped on the floor (defense: a caller that somehow handed back a
// resized buffer must not pollute the pool).
func (p *BufPool) Put(b *[]byte) {
	if b == nil || len(*b) != p.pageSize {
		return
	}
	clear(*b)
	p.pool.Put(b)
}

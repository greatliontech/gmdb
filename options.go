// Package gmdb is a memory-mapped, multi-process, embedded key-value
// database for Go 1.24+.
//
// This file scopes Options to the chunk-1 acceptance surface only: page
// size, checksum, file-size bounds, and the slab budget. The full
// option set in api-surface.md lands incrementally as later chunks
// require it (SyncMode, MaxReaders, RestartGroupTarget, etc.).
package gmdb

import (
	"cmp"
	"crypto/rand"

	"github.com/thegrumpylion/gmdb/internal/page"
)

// Options configures a fresh database at creation time. For an existing
// database the persisted meta is authoritative; Options is consulted
// only for the runtime fields (MaxTxBufferBytes, ReadOnly) that have no
// on-disk counterpart.
type Options struct {
	// PageSize is set at creation, immutable afterwards. Must be a
	// power of two in [4 KB, 64 KB]. Default 4096.
	PageSize uint32

	// PageChecksum enables the xxhash64 page-footer on every data
	// page. Set at creation, immutable. Default true.
	PageChecksum bool

	// MinSize, MaxSize, GrowStep, ShrinkThreshold control file
	// growth and shrinkage in pages. MaxSize is immutable after
	// creation. Defaults: MinSize=64, MaxSize=4_194_304 (16 GiB at
	// 4 KB), GrowStep=64, ShrinkThreshold=128.
	MinSize         uint64
	MaxSize         uint64
	GrowStep        uint64
	ShrinkThreshold uint64

	// MaxTxBufferBytes bounds the per-transaction slab. Exceeding
	// this returns ErrTxTooLarge. Default 256 MiB.
	MaxTxBufferBytes int

	// UUID may be supplied for deterministic database identity in
	// tests; if zero, a random UUID is generated at creation.
	UUID [16]byte
}

func (o Options) applyDefaults() Options {
	o.PageSize = cmp.Or(o.PageSize, uint32(4096))
	o.MinSize = cmp.Or(o.MinSize, uint64(64))
	o.MaxSize = cmp.Or(o.MaxSize, uint64(4_194_304))
	o.GrowStep = cmp.Or(o.GrowStep, uint64(64))
	o.ShrinkThreshold = cmp.Or(o.ShrinkThreshold, uint64(128))
	o.MaxTxBufferBytes = cmp.Or(o.MaxTxBufferBytes, 256<<20)
	if o.UUID == ([16]byte{}) {
		_, _ = rand.Read(o.UUID[:])
	}
	return o
}

func (o Options) validate() error {
	if !page.ValidPageSize(o.PageSize) {
		return errInvalidPageSize
	}
	if o.MaxSize == 0 || o.MinSize > o.MaxSize {
		return errInvalidSizeBounds
	}
	if o.MaxTxBufferBytes <= 0 {
		return errInvalidTxBuffer
	}
	return nil
}

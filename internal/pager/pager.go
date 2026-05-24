package pager

import (
	"errors"
	"fmt"
	"os"
	"slices"

	"github.com/thegrumpylion/gmdb/internal/bitmap"
	"github.com/thegrumpylion/gmdb/internal/page"
)

// Errors surfaced by the pager. Sentinels so callers can use errors.Is /
// errors.As.
var (
	// ErrTxTooLarge is returned by CoW and by commit-step-0 assembly when
	// allocating another page-sized buffer would push the transaction's
	// slab usage past MaxTxBufferBytes.
	ErrTxTooLarge = errors.New("pager: transaction buffer budget exceeded")

	// ErrReadOnly is returned by mutating methods on a read-only pager.
	ErrReadOnly = errors.New("pager: read-only pager rejects mutation")

	// ErrPageNotDirty is returned by Mutate on a page that has not been
	// CoW'd in this transaction. The caller must CoW first.
	ErrPageNotDirty = errors.New("pager: page is not CoW'd in this transaction")

	// ErrDBFull is returned by AllocPage when no free page is available
	// from any priority tier (loose, bitmap, RPL reclamation, file
	// extension up to MaxSize).
	ErrDBFull = errors.New("pager: database is full")

	// ErrFreespaceUnconfigured is returned by AllocPage / FreePage /
	// TailRefund when the writer has not yet been seeded with bitmap +
	// commit state via AttachBitmap and SetCommitState.
	ErrFreespaceUnconfigured = errors.New("pager: freespace state not configured")

	// ErrCorrupted is returned when a structural integrity violation
	// is detected, either on-disk (Open: malformed RPL segment, chain
	// cycle, meta payload invalid, dual-meta selection ambiguous) or
	// in the about-to-be-written commit assembly buffer (Commit:
	// pre-write defense-in-depth checks that catch reservation bugs
	// that would produce on-disk corruption if encoded). The
	// caller-side recovery is the same regardless of where the
	// integrity failure was caught — discard the handle, re-Open if
	// possible — so a single sentinel covers both. The root package's
	// mapPagerErr translates this into gmdb.ErrCorrupted, the public
	// api-surface.md sentinel; the descriptive message is preserved
	// in the wrapped chain.
	ErrCorrupted = errors.New("pager: structural corruption detected")
)

// Pager resolves page bytes for one transaction. Read transactions get a
// read-only pager (mmap-only resolution). Write transactions get a writable
// pager that additionally owns the dirty-page slab (`dirty[id]` checked
// first, then mmap) and the buffer pool reservation.
//
// One Pager instance is bound to one transaction; concurrent calls on the
// same Pager are not allowed (the writer is single-threaded by design, and
// read pagers are owned by exactly one read tx).
type Pager struct {
	cfg      page.Config
	file     *os.File
	mmap     []byte
	fileSize int64
	closed   bool

	// Write-side state. Nil/zero on a read-only pager.
	dirty      map[uint64]*[]byte
	dirtyBytes int
	maxBytes   int
	bufPool    *BufPool
	readOnly   bool

	// Freespace state machine. Populated by AttachBitmap +
	// SetCommitState + SetRPLChain at the start of the write
	// transaction; nil/empty on a read-only pager.
	bitmap           *bitmap.Bitmap
	rplSegments      []RPLSegmentRef
	highWaterMark    uint64
	maxSizePages     uint64
	reclamationBound uint64

	pendingAllocs map[uint64]struct{}
	pendingFrees  map[uint64]struct{}
	retiredPages  []uint64
	loosePages    map[uint64]struct{}

	// detachedBufs holds slab buffers that were severed from
	// p.dirty when their page id was loose-popped by AllocPage.
	// Required by the chunk-5.4 fix to the loose-page reuse contract:
	// the original-tx caller of the slab buffer holds a borrowed
	// []byte (byte-slice ownership valid through tx close), so the
	// buffer must stay alive; but a fresh CoW / AllocSlab on the
	// loose-popped id needs to install a NEW buffer (not return the
	// stale one via CoW's idempotent re-CoW shortcut). Detach moves
	// the old buffer here so the new buffer can land in p.dirty.
	// ReleaseAll / AbortTx pool-Put every detached buffer alongside
	// the live p.dirty buffers.
	detachedBufs []*[]byte

	// currentTxnID is the TxnID of the in-progress write transaction.
	// Set by SetCurrentTxnID; consumed by commit-step-0's RPL segment
	// builder. Zero on read-only pagers and between transactions on
	// writable pagers.
	currentTxnID uint64

	// bitmapSnapshot captures the bitmap's mutable state at the start
	// of the in-progress write tx so AbortTx can restore it. Set by
	// BeginTx, cleared by Commit success or by AbortTx. Nil between
	// transactions.
	bitmapSnapshot *bitmap.Snapshot

	// hwmSnapshot / rplChainSnapshot capture the same state for the
	// pager-side bookkeeping so rollback restores all tx-mutated state
	// — not just the bitmap. Without this, file extension that
	// advanced HighWaterMark and RPL reclamation that popped segments
	// from the in-memory chain would persist across a rollback.
	hwmSnapshot      uint64
	rplChainSnapshot []RPLSegmentRef
	haveTxSnapshot   bool

	// commitStep4HookForTest is a test-only injection point. When
	// non-nil, Commit invokes it after step 3 has written the new
	// meta to disk but before step 4's fdatasync, treating a returned
	// error as if fdatasync itself failed. Used by the root package's
	// poison-on-publication-failure regression test to simulate the
	// step-3-success / step-4-fail window without mocking the file.
	// Production callers must not set this.
	commitStep4HookForTest func() error

	// laggingReader is the chunk-5.5 LaggingReader callback per
	// lock-ordering.md §Lagging Reader Handling and free-space.md
	// §Page Allocation Priority step 4. Invoked when AllocPage /
	// AllocContiguous detect bitmap-exhausted AND RPL-reclaim-
	// blocked-by-bound. At most once per AllocPage call to avoid
	// busy loops.
	laggingReader func(LaggingReaderInfo) LaggingReaderAction

	// refreshReclamationBound is the chunk-5.5 plumb-through for
	// "go re-poll the reader table and re-compute the bound." Used
	// after LaggingReaderWait. DB.Begin captures coord and supplies
	// a closure that re-derives the bound from coord.OldestReaderTxnID
	// + the prev meta's TxnID. nil ⇒ no refresh attempted (the
	// pager falls through to file extension after Wait).
	refreshReclamationBound func() uint64
}

// LaggingReaderInfo is the pager-side mirror of gmdb.LaggingReaderInfo.
// Passed to the chunk-5.5 callback by AllocPage / AllocContiguous when
// reclamation is blocked by a reader. PID/HeldPages are zero when the
// pager cannot cheaply derive them — the chunk-5.5 wiring fills only
// TxnID and Lag from local state.
type LaggingReaderInfo struct {
	PID       uint32
	TxnID     uint64
	Lag       uint64
	HeldPages uint64
}

// LaggingReaderAction is the return type of the callback.
type LaggingReaderAction int

const (
	// LaggingReaderWait directs AllocPage to refresh and retry.
	LaggingReaderWait LaggingReaderAction = iota
	// LaggingReaderAbort directs AllocPage to return ErrDBFull.
	LaggingReaderAbort
)

// RPLSegmentRef is the in-memory descriptor of one on-disk RPL segment
// page. The pager maintains a slice of these ordered tail (index 0,
// oldest) → head (last, newest) per `free-space.md §RPL in-memory
// segment list`. Rebuilt at Open by walking the on-disk chain head →
// tail and reversing.
type RPLSegmentRef struct {
	PageID uint64
	TxnID  uint64
	Count  uint32 // number of PageID entries this segment carries
}

// NewReader opens a read-only pager over file. The data file is mapped
// MAP_SHARED|PROT_READ with mprotect(PROT_READ) re-applied as a belt-and-
// suspenders guard. reservationBytes is the address-space reservation —
// per mmap-strategy.md it is sized to MaxSize regardless of the file's
// current length; accesses beyond the file's HighWaterMark SIGBUS the
// process (callers must honour the active meta's HighWaterMark).
//
// The pager retains file and closes the mmap on Close. The caller is
// responsible for closing file (the pager does not own it — multiple
// pagers may share the same file handle through a DB).
func NewReader(file *os.File, cfg page.Config, reservationBytes int64) (*Pager, error) {
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("pager: %w", err)
	}
	if reservationBytes <= 0 {
		return nil, fmt.Errorf("pager: reservation must be > 0")
	}
	mapping, err := mmapRO(file.Fd(), reservationBytes)
	if err != nil {
		return nil, fmt.Errorf("pager: mmap: %w", err)
	}
	if err := mprotectRO(mapping); err != nil {
		_ = munmap(mapping)
		return nil, fmt.Errorf("pager: mprotect: %w", err)
	}
	st, err := file.Stat()
	if err != nil {
		_ = munmap(mapping)
		return nil, fmt.Errorf("pager: stat: %w", err)
	}
	return &Pager{
		cfg:      cfg,
		file:     file,
		mmap:     mapping,
		fileSize: st.Size(),
		readOnly: true,
	}, nil
}

// NewWriter opens a writable pager over file. Same mmap setup as NewReader;
// additionally allocates the dirty-page slab. pool must be a process-wide
// BufPool sized to cfg.PageSize. maxBytes is Options.MaxTxBufferBytes; CoW
// and commit-step-0 buffer allocation respect this bound and return
// ErrTxTooLarge when crossing it.
func NewWriter(file *os.File, cfg page.Config, reservationBytes int64, pool *BufPool, maxBytes int) (*Pager, error) {
	if pool == nil {
		return nil, fmt.Errorf("pager: pool must not be nil")
	}
	if pool.PageSize() != int(cfg.PageSize) {
		return nil, fmt.Errorf("pager: pool page size %d != cfg %d", pool.PageSize(), cfg.PageSize)
	}
	if maxBytes <= 0 {
		return nil, fmt.Errorf("pager: maxBytes must be > 0")
	}
	p, err := NewReader(file, cfg, reservationBytes)
	if err != nil {
		return nil, err
	}
	p.dirty = make(map[uint64]*[]byte)
	p.maxBytes = maxBytes
	p.bufPool = pool
	p.readOnly = false
	p.pendingAllocs = make(map[uint64]struct{})
	p.pendingFrees = make(map[uint64]struct{})
	p.loosePages = make(map[uint64]struct{})
	return p, nil
}

// AttachBitmap installs the in-memory allocation bitmap for write-side
// freespace operations. Required before AllocPage / FreePage / TailRefund.
// Read-only pagers do not call this.
func (p *Pager) AttachBitmap(bm *bitmap.Bitmap) { p.bitmap = bm }

// Bitmap returns the attached bitmap (nil on read-only or unconfigured
// writable pagers). Used by tests and commit-step-0 to enumerate dirty
// bitmap pages for pwrite.
func (p *Pager) Bitmap() *bitmap.Bitmap { return p.bitmap }

// SetCommitState seeds the pager's snapshot of meta-state at the start
// of a write transaction:
//   - highWaterMark: first unallocated page ID from the active meta.
//     File extension advances this; tail refund decrements it.
//   - maxSizePages: MaxSize / PageSize from the active meta. Caps file
//     growth; AllocPage returns ErrDBFull when extension would exceed
//     this.
//   - reclamationBound: min(oldestActiveReaderTxnID, lastCheckpointTxnID).
//     RPL entries with TxnID strictly less than this bound are
//     reclaimable. Chunk 1 has no cross-process reader scan and no
//     SyncLazy support, so callers pass the previous TxnID + 1 (i.e.
//     every prior commit is a checkpoint and there are no other
//     processes).
func (p *Pager) SetCommitState(highWaterMark, maxSizePages, reclamationBound uint64) {
	p.highWaterMark = highWaterMark
	p.maxSizePages = maxSizePages
	p.reclamationBound = reclamationBound
}

// SetRPLChain seeds the in-memory RPL segment list. segments is ordered
// tail (index 0, oldest TxnID) → head (last, newest TxnID).
func (p *Pager) SetRPLChain(segments []RPLSegmentRef) {
	p.rplSegments = append(p.rplSegments[:0], segments...)
}

// RPLChain returns the current in-memory segment list. Used by commit
// step 0 to pwrite newly-appended segment pages and by chunk-11
// integrity check.
func (p *Pager) RPLChain() []RPLSegmentRef { return p.rplSegments }

// BeginTx snapshots the pager's mutable state (bitmap, HighWaterMark,
// RPL chain) so AbortTx can restore it. Called at the start of every
// write transaction. Idempotent: a second call clobbers the first
// snapshot (used by tests; production callers don't re-Begin without
// Commit/Rollback).
func (p *Pager) BeginTx() {
	if p.readOnly || p.bitmap == nil {
		return
	}
	p.bitmapSnapshot = p.bitmap.Snapshot()
	p.hwmSnapshot = p.highWaterMark
	p.rplChainSnapshot = slices.Clone(p.rplSegments)
	p.haveTxSnapshot = true
}

// AbortTx restores the snapshotted state, releases slab buffers, and
// resets tx-scoped freespace bookkeeping. The on-disk state is
// unaffected (no pwrites have occurred — AbortTx is called on tx
// rollback or on commit-pipeline failure, both of which guarantee no
// data-page pwrites have completed).
//
// After AbortTx returns, the pager is ready to start a fresh
// transaction via BeginTx + SetCommitState.
func (p *Pager) AbortTx() {
	if p.readOnly {
		return
	}
	if p.haveTxSnapshot && p.bitmap != nil {
		p.bitmap.Restore(p.bitmapSnapshot)
		p.highWaterMark = p.hwmSnapshot
		p.rplSegments = slices.Clone(p.rplChainSnapshot)
	}
	p.bitmapSnapshot = nil
	p.rplChainSnapshot = nil
	p.haveTxSnapshot = false
	p.ReleaseAll()
	clear(p.pendingAllocs)
	clear(p.pendingFrees)
	clear(p.loosePages)
	p.retiredPages = p.retiredPages[:0]
	p.currentTxnID = 0
}

// discardTxSnapshot drops the snapshot without restoring (Commit
// success path).
func (p *Pager) discardTxSnapshot() {
	p.bitmapSnapshot = nil
	p.rplChainSnapshot = nil
	p.haveTxSnapshot = false
}

// HighWaterMark returns the current write-tx HighWaterMark snapshot.
func (p *Pager) HighWaterMark() uint64 { return p.highWaterMark }

// PendingAllocs / PendingFrees / RetiredPages expose the tx-scoped
// bookkeeping for the commit-step-0 caller (chunk 1.8). Returned
// slices/maps are owned by the pager — callers must not mutate.
func (p *Pager) PendingAllocs() map[uint64]struct{} { return p.pendingAllocs }
func (p *Pager) PendingFrees() map[uint64]struct{}  { return p.pendingFrees }
func (p *Pager) RetiredPages() []uint64             { return p.retiredPages }
func (p *Pager) LoosePages() map[uint64]struct{}    { return p.loosePages }

// Close releases the mmap region. Idempotent. The pager does not close
// file — that is the DB's responsibility.
func (p *Pager) Close() error {
	if p.closed {
		return nil
	}
	p.closed = true
	if p.mmap == nil {
		return nil
	}
	err := munmap(p.mmap)
	p.mmap = nil
	return err
}

// Config returns the page configuration.
func (p *Pager) Config() page.Config { return p.cfg }

// SetCommitStep4HookForTest installs the test-only step-4-failure
// injection hook described on the Pager struct's commitStep4HookForTest
// field. Pass nil to clear. Test-only — production callers must not
// use this.
func (p *Pager) SetCommitStep4HookForTest(fn func() error) {
	p.commitStep4HookForTest = fn
}

// SetLaggingReaderCallback installs the chunk-5.5 LaggingReader
// callback. nil clears. AllocPage / AllocContiguous invoke this when
// bitmap-exhausted AND reclamation-blocked-by-bound. At most once per
// AllocPage call.
func (p *Pager) SetLaggingReaderCallback(cb func(LaggingReaderInfo) LaggingReaderAction) {
	p.laggingReader = cb
}

// SetReclamationBoundRefresh installs the bound-refresh closure used
// after LaggingReaderWait. nil clears (no refresh, fall through to
// file extension). DB.Begin captures coord and supplies a closure
// that recomputes min(coord.OldestReaderTxnID(), prevMeta.TxnID).
func (p *Pager) SetReclamationBoundRefresh(refresh func() uint64) {
	p.refreshReclamationBound = refresh
}

// FileSize returns the file size observed at Open. Used to bound reads
// (callers must additionally respect HighWaterMark from the active meta).
func (p *Pager) FileSize() int64 { return p.fileSize }

// DirtyBytes returns the current slab usage in bytes (writable pagers
// only; returns 0 on read-only pagers).
func (p *Pager) DirtyBytes() int { return p.dirtyBytes }

// MaxBytes returns the slab budget (writable pagers only).
func (p *Pager) MaxBytes() int { return p.maxBytes }

// IsReadOnly reports whether mutating operations are rejected.
func (p *Pager) IsReadOnly() bool { return p.readOnly }

// Page returns a borrowed byte slice for the page at id. Resolution
// order: dirty[id] (write txn only) then mmap.
//
// The returned slice has length cfg.PageSize and is valid as long as the
// transaction owns the pager (the slab buffer survives loose-page
// retirement until Commit/Rollback per the byte-slice ownership
// invariant in pager-slab.md). For mmap-backed reads the slice is
// valid until the mmap is unmapped (i.e. Close()).
//
// Panics if id*PageSize would exceed the mmap reservation. Callers must
// gate access by HighWaterMark from the active meta.
func (p *Pager) Page(id uint64) []byte {
	if !p.readOnly {
		if buf, ok := p.dirty[id]; ok {
			return *buf
		}
	}
	off := id * uint64(p.cfg.PageSize)
	end := off + uint64(p.cfg.PageSize)
	if end > uint64(len(p.mmap)) {
		panic(fmt.Sprintf("pager: Page(%d) past mmap reservation [%d, %d]", id, off, end))
	}
	return p.mmap[off:end]
}

// CoW installs a fresh slab buffer at dstID populated from the current
// content of srcID. dstID is supplied by the caller's allocator (see
// free-space.md §Page Allocation Priority — wired in chunk 1.7); srcID
// is the page being CoW'd from (may be mmap-backed or already in
// p.dirty).
//
// Returns the new buffer for the caller to mutate. Returns ErrTxTooLarge
// if installing the buffer would exceed MaxTxBufferBytes; ErrReadOnly on
// a read-only pager. The buffer remains owned by the pager until
// Commit/Rollback or an explicit Discard.
//
// Idempotence: if dstID is already in p.dirty (re-CoW'd within the same
// transaction), CoW returns the existing buffer unchanged. This honours
// the "re-modifying a page already CoW'd mutates the existing slab
// buffer in place" invariant.
func (p *Pager) CoW(srcID, dstID uint64) ([]byte, error) {
	if p.readOnly {
		return nil, ErrReadOnly
	}
	if existing, ok := p.dirty[dstID]; ok {
		// Idempotent re-CoW: same destination already owned by this tx.
		return *existing, nil
	}
	if p.dirtyBytes+int(p.cfg.PageSize) > p.maxBytes {
		return nil, ErrTxTooLarge
	}
	src := p.Page(srcID)
	buf := p.bufPool.Get()
	copy(*buf, src)
	p.dirty[dstID] = buf
	p.dirtyBytes += int(p.cfg.PageSize)
	return *buf, nil
}

// AllocSlab installs a fresh zero-filled slab buffer at id without
// reading any source page. Used by commit step 0 to assemble RPL segment
// pages, modified bitmap pages, and similar structures that have no
// prior on-disk content for this transaction. Returns ErrTxTooLarge on
// budget overrun.
//
// Idempotent: if a buffer already exists at id, the existing buffer is
// returned and no further accounting occurs.
func (p *Pager) AllocSlab(id uint64) ([]byte, error) {
	if p.readOnly {
		return nil, ErrReadOnly
	}
	if existing, ok := p.dirty[id]; ok {
		return *existing, nil
	}
	if p.dirtyBytes+int(p.cfg.PageSize) > p.maxBytes {
		return nil, ErrTxTooLarge
	}
	buf := p.bufPool.Get()
	p.dirty[id] = buf
	p.dirtyBytes += int(p.cfg.PageSize)
	return *buf, nil
}

// AllocSlabRun installs n fresh zero-filled slab buffers covering the
// contiguous run [firstID, firstID+n) previously reserved via
// AllocContiguous. pages[i] is the buffer for firstID + uint64(i).
// Implements the chunk-4.7 PageWriter contract used by the
// overflow-chain Put path (internal/btree.overflow).
//
// Atomicity: the slab budget is checked once against the full n*PageSize
// before any buffer is installed. On budget exceed (ErrTxTooLarge),
// nothing is installed — the caller is expected to FreeRun(firstID, n)
// to roll back the prior AllocContiguous.
//
// Idempotence: per-page, AllocSlab semantics are preserved — a buffer
// already installed at any id in the run is returned unchanged and no
// budget is charged for that id. The pre-flight budget check uses the
// count of NOT-already-installed pages so an idempotent re-run of the
// same firstID does not double-bill.
func (p *Pager) AllocSlabRun(firstID uint64, n uint32) ([][]byte, error) {
	if p.readOnly {
		return nil, ErrReadOnly
	}
	if n == 0 {
		return nil, fmt.Errorf("pager: AllocSlabRun: n must be > 0")
	}
	end := firstID + uint64(n)
	fresh := int64(0)
	for id := firstID; id < end; id++ {
		if _, ok := p.dirty[id]; !ok {
			fresh++
		}
	}
	// int64 arithmetic on the budget check so GOARCH=386/arm overflow
	// isn't reachable for large n (uint32 max × 64 KB PageSize ≈ 2^48).
	if int64(p.dirtyBytes)+fresh*int64(p.cfg.PageSize) > int64(p.maxBytes) {
		return nil, ErrTxTooLarge
	}
	out := make([][]byte, n)
	for i := uint32(0); i < n; i++ {
		id := firstID + uint64(i)
		if existing, ok := p.dirty[id]; ok {
			out[i] = *existing
			continue
		}
		buf := p.bufPool.Get()
		p.dirty[id] = buf
		p.dirtyBytes += int(p.cfg.PageSize)
		out[i] = *buf
	}
	return out, nil
}

// Mutate returns the writable slab buffer at id. Returns ErrPageNotDirty
// if id has not been CoW'd or AllocSlab'd in this transaction (the
// caller must CoW first); ErrReadOnly on a read-only pager. The returned
// slice is the same backing memory CoW returned — mutations are visible
// to subsequent Page(id) reads.
func (p *Pager) Mutate(id uint64) ([]byte, error) {
	if p.readOnly {
		return nil, ErrReadOnly
	}
	buf, ok := p.dirty[id]
	if !ok {
		return nil, ErrPageNotDirty
	}
	return *buf, nil
}

// IsDirty reports whether id has a slab buffer in this transaction.
func (p *Pager) IsDirty(id uint64) bool {
	if p.readOnly {
		return false
	}
	_, ok := p.dirty[id]
	return ok
}

// Discard removes id's slab buffer from the dirty map and returns it to
// the pool. No-op if id is not dirty. Used by rollback and by
// commit-step-0 to drop entries that the assembly phase decided are no
// longer needed (e.g. a CoW that becomes loose and then folded into
// pendingFrees).
//
// Note: per the byte-slice ownership invariant, callers must not Discard
// a buffer whose contents have been handed to the user as a borrowed
// []byte until tx close. The freespace state machine (chunk 1.7)
// enforces this by only Discarding at commit / rollback time.
func (p *Pager) Discard(id uint64) {
	if p.readOnly {
		return
	}
	buf, ok := p.dirty[id]
	if !ok {
		return
	}
	delete(p.dirty, id)
	p.dirtyBytes -= int(p.cfg.PageSize)
	p.bufPool.Put(buf)
}

// ReleaseAll returns every slab buffer to the pool and empties the
// dirty map. Used by commit (after pwrites complete) and by rollback.
// Also drains p.detachedBufs (buffers detached from p.dirty by the
// AllocPage loose-pop path — see the chunk-5.4 fix comment on
// detachedBufs). The pager is left in a state where Page() returns
// mmap-only views.
func (p *Pager) ReleaseAll() {
	if p.readOnly {
		return
	}
	if len(p.dirty) == 0 && len(p.detachedBufs) == 0 {
		return
	}
	for _, buf := range p.dirty {
		p.bufPool.Put(buf)
	}
	p.dirty = make(map[uint64]*[]byte)
	for _, buf := range p.detachedBufs {
		p.bufPool.Put(buf)
	}
	p.detachedBufs = p.detachedBufs[:0]
	p.dirtyBytes = 0
}

// DirtyIDs returns a snapshot of the page IDs currently in the slab.
// Order is unspecified. Used by commit step 1 to iterate the pwrite set.
func (p *Pager) DirtyIDs() []uint64 {
	if p.readOnly || len(p.dirty) == 0 {
		return nil
	}
	out := make([]uint64, 0, len(p.dirty))
	for id := range p.dirty {
		out = append(out, id)
	}
	return out
}

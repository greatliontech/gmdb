package pager

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"sync/atomic"

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

	// ErrVersionMismatch is returned by Open / DiscoverPageSize when
	// meta-0 is an intact gmdb meta (checksum + Magic valid) whose
	// Version differs from page.FormatVersion: a valid gmdb file written
	// by a different, unreadable on-disk format version — distinct from
	// corruption. The root package's mapPagerErr translates this into
	// gmdb.ErrVersionMismatch.
	ErrVersionMismatch = errors.New("pager: on-disk format version mismatch")

	// ErrBadPageChecksum is returned by Page when a data page's xxhash64
	// footer does not match its content (checksums.md §Verification). Distinct from ErrCorrupted: a checksum mismatch is silent
	// bitrot on a structurally-plausible page, whereas ErrCorrupted is a
	// structural-layout violation. mapPagerErr translates this into
	// gmdb.ErrBadPageChecksum, the public api-surface.md sentinel.
	ErrBadPageChecksum = errors.New("pager: page checksum mismatch")
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

	// inCommit marks the commit phase (the db layer's descriptor
	// flush + Commit's assembly/pwrite steps). While set, slab
	// admission checks against the raw maxBytes instead of
	// maxBytes − reserve: the commit sequence's allocations are
	// exactly what the reserve was maintained for, so they draw from
	// the reserved space. The raw-cap check remains as the spec's
	// backstop (pager-slab.md §Slab Budget: ErrTxTooLarge fires on a
	// commit-step-0 allocation that would exceed the bound). Set via
	// SetCommitPhase and by Commit itself.
	inCommit bool

	// externalReserve is the db layer's commit-reserve contribution
	// (descriptor-flush projection); see SetExternalReserve. Reset per
	// write transaction by BeginTx.
	externalReserve int

	// Cold-tracking for Options.ReclaimOnClose (mmap-strategy.md §Read
	// Transaction Cooldown). Enabled per read-tx pager via
	// EnableColdTracking; Page / pageRaw then record the lowest/highest
	// page IDs the transaction touches, and AdviseColdAccessed issues a
	// single MADV_COLD over [accessMin, accessMax] at close. The
	// counters are atomic so concurrent reads off one ReadTx are safe;
	// trackCold is set once before any read (no concurrent mutation).
	// The min/max pair is not updated jointly-atomically, but
	// AdviseColdAccessed loads both only at close — after every read has
	// quiesced (the held CAS) — so the final range it advises is exact.
	// Off (trackCold == false) on every other pager — one branch per
	// page read when disabled. accessMin is seeded to MaxUint64 so the
	// first access wins the min CAS; accessMin > accessMax means "no
	// page accessed" and suppresses the MADV_COLD.
	trackCold bool
	accessMin atomic.Uint64
	accessMax atomic.Uint64

	// tc holds the per-write-transaction activity counters that back
	// TxStats (see txstats.go). Reset by BeginTx, read via
	// TxStatsSnapshot. Unused on a read-only pager.
	tc txCounters

	// Freespace state machine. Populated by AttachBitmap +
	// SetCommitState + SetRPLChain at Open/Resync and re-seeded per
	// write transaction by BeginTx(TxParams); nil/empty on a
	// read-only pager.
	bitmap           *bitmap.Bitmap
	rplSegments      []RPLSegmentRef
	highWaterMark    uint64
	maxSizePages     uint64
	growStepPages    uint64 // GrowStep: file grows in this-aligned increments (file-format.md §File Growth). 0 ⇒ grow by exact need.
	minSizePages     uint64 // MinSize: shrink never truncates below this (file-format.md §File Shrinkage). 0 ⇒ no floor.
	reclamationBound uint64

	// rplCorruptCount counts RPL-segment quarantine occurrences this
	// session (a torn segment re-encountered after an aborted write tx
	// counts again; the signal is "corruption present", not an exact
	// distinct-segment count) because they failed to decode (free-space.md
	// §RPL Reclamation). Surfaced via RPLCorruptCount → DBStats so a
	// long-running embedder can detect corruption without an explicit
	// Check(). rplCorruptCb, if set, is invoked once per quarantined
	// segment for an immediate log.
	rplCorruptCount uint64
	rplCorruptCb    func(segPageID uint64)

	pendingAllocs map[uint64]struct{}
	pendingFrees  map[uint64]struct{}
	retiredPages  []uint64
	loosePages    map[uint64]struct{}

	// detachedBufs holds slab buffers that were severed from
	// p.dirty when their page id was loose-popped by AllocPage.
	// Required by the loose-page reuse contract:
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
	// Seeded by BeginTx(TxParams) (re-seedable via SetCurrentTxnID);
	// consumed by commit-step-0's RPL segment builder. Zero on
	// read-only pagers and between transactions on writable pagers.
	currentTxnID uint64

	// savepointDepth counts active (unresolved) NESTED-kind savepoints
	// (transactions.md §Nested Transactions). While > 0, AllocPage
	// suspends loose-page reuse so a child cannot hand out a page id
	// whose slab buffer an ancestor's tree still references (pager-slab.md §Slab Lifecycle Across Nested Transactions).
	// Managed by BeginSavepoint / RestoreSavepoint / ReleaseSavepoint;
	// reset to 0 by AbortTx. See savepoint.go.
	//
	// SHALLOW-kind savepoints (BeginShallowSavepoint) do NOT increment
	// this depth — they exist for internal-helper atomicity (per-row
	// indexed Put/Delete maintenance, see indexing.md §Write Path) and
	// must allow loose-pop to remain enabled so across-Put loose-page
	// recycling stays bounded. Shallow savepoints track loose-pop
	// events explicitly in Savepoint.loosePopLog so Restore can
	// re-attach the detached buffer rather than relying on the
	// suspended-loose-pop invariant.
	savepointDepth int

	// activeSavepoints is the LIFO stack of unresolved savepoints of
	// BOTH kinds (Nested and Shallow). Pushed by Begin{,Shallow}
	// Savepoint and popped by Restore/Release in strict-LIFO order.
	//
	// Two uses:
	//
	//  1. recordSavepointUndo's fast-path skips appending to
	//     savepointUndoLog when this stack is empty — mirroring
	//     bitmap.recordFlip's openSnapshots == 0 guard.
	//
	//  2. AllocPage's loose-pop branch appends an (id, original-buffer)
	//     entry to the loosePopLog of each Shallow-kind entry on this
	//     stack (filtered by kind == SavepointShallow). Loose-pop is
	//     suspended during Nested windows (savepointDepth > 0), so a
	//     Nested-only stack does not see loose-pop events.
	//
	// Nil/empty between savepoints; reset on AbortTx alongside
	// savepointDepth.
	activeSavepoints []*Savepoint

	// savepointUndoLog is the shared per-tx undo log into which every
	// state-changing mutation of the tx-scoped pending sets and dirty
	// additions appends an entry while at least one savepoint is open
	// (see recordSavepointUndo + savepointUndoEntry). Each Savepoint
	// records its capture-time position (Savepoint.undoLogPos);
	// RestoreSavepoint replays log[sp.undoLogPos:end] in reverse and
	// then truncates to sp.undoLogPos.
	//
	// Cost contract (transactions.md §Nested Transactions): the log
	// grows by O(1) per actual mutation observed while any savepoint is
	// open, never with cumulative tx state or MaxSize. Truncates to
	// length 0 in ReleaseSavepoint when the last open savepoint is
	// released, in AbortTx (top-level rollback), and in
	// discardTxSnapshot (commit success) — so it never survives a tx
	// boundary. The bitmap layer's per-Snapshot undo log (0893be5) is
	// the structural parent of this design.
	savepointUndoLog []savepointUndoEntry

	// verified is the per-transaction checksum-verification cache
	// (checksums.md §Verification): a mmap page whose xxhash64
	// footer has been verified once in this tx is recorded here and not
	// re-verified on subsequent accesses within the tx. A compact
	// []uint64 bitset indexed by page id, lazily allocated on the first
	// verified read and reset per write tx (BeginTx / AbortTx); a read
	// pager builds it fresh per ReadTx and discards it at close. Nil when
	// PageChecksum is disabled or before the first verified read.
	verified []uint64

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

	// laggingReader is the LaggingReader callback per
	// lock-ordering.md §Lagging Reader Handling and free-space.md
	// §Page Allocation Priority step 4. Invoked when AllocPage /
	// AllocContiguous detect bitmap-exhausted AND RPL-reclaim-
	// blocked-by-bound. At most once per AllocPage call to avoid
	// busy loops.
	laggingReader func(LaggingReaderInfo) LaggingReaderAction

	// refreshReclamationBound is the plumb-through for
	// "go re-poll the reader table and re-compute the bound." Used
	// after LaggingReaderWait. DB.Begin captures coord and supplies
	// a closure that re-derives the bound from coord.OldestReaderTxnID
	// + the prev meta's TxnID. nil ⇒ no refresh attempted (the
	// pager falls through to file extension after Wait).
	refreshReclamationBound func() uint64

	// contigAttempts / contigFragFails instrument the contiguous-
	// allocation failure rate that drives incremental compaction
	// (background-maintenance.md §Incremental Compaction Trigger).
	// contigAttempts counts multi-page AllocContiguous calls (n > 1);
	// contigFragFails counts those whose first bitmap scan found no
	// contiguous run despite sufficient total free pages (fragmentation,
	// not fullness). atomic.Uint64 because the writer increments them
	// (single-threaded) while the maintenance goroutine reads+resets via
	// ConsumeContiguousAllocStats. They survive across write txns on this
	// (long-lived) writer pager — BeginTx does not reset them — and are
	// naturally dropped when Compact swaps in a fresh pager (post-compact
	// fragmentation is reduced, so a fresh window is correct). A miscount
	// only mis-schedules compaction; it is never a correctness hazard.
	contigAttempts  atomic.Uint64
	contigFragFails atomic.Uint64
}

// LaggingReaderInfo is the pager-side mirror of gmdb.LaggingReaderInfo.
// Passed to the LaggingReader callback by AllocPage / AllocContiguous when
// reclamation is blocked by a reader. PID/HeldPages are zero when the
// pager cannot cheaply derive them — the wiring fills only
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

// SetCommitState installs the committed meta-state baseline outside a
// write transaction — Open/Resync (attachState) after a rebuild, and
// test fixtures that exercise allocation without a tx:
//   - highWaterMark: first unallocated page ID from the active meta.
//     File extension advances this; tail refund decrements it.
//   - maxSizePages: MaxSize (in pages) from the active meta. Caps file
//     growth; AllocPage returns ErrDBFull when extension would exceed
//     this.
//   - reclamationBound: min(oldestActiveReaderTxnID, lastCheckpointTxnID).
//     RPL entries with TxnID strictly less than this bound are
//     reclaimable.
//
// Per-write-tx seeding goes through BeginTx(TxParams), which
// re-installs all three from the transaction's view.
func (p *Pager) SetCommitState(highWaterMark, maxSizePages, reclamationBound uint64) {
	p.highWaterMark = highWaterMark
	p.maxSizePages = maxSizePages
	p.reclamationBound = reclamationBound
}

// setSizeParams seeds the file growth/shrink parameters from the active
// meta (GrowStep, MinSize — both in pages). Called from Open/Resync
// (attachState); per write tx the same fields arrive via
// BeginTx(TxParams). file-format.md §File Growth / §File Shrinkage.
func (p *Pager) setSizeParams(growStepPages, minSizePages uint64) {
	p.growStepPages = growStepPages
	p.minSizePages = minSizePages
}

// SetRPLChain seeds the in-memory RPL segment list. segments is ordered
// tail (index 0, oldest TxnID) → head (last, newest TxnID).
func (p *Pager) SetRPLChain(segments []RPLSegmentRef) {
	p.rplSegments = append(p.rplSegments[:0], segments...)
}

// RPLChain returns the current in-memory segment list. Used by commit
// step 0 to pwrite newly-appended segment pages and by the
// integrity check.
func (p *Pager) RPLChain() []RPLSegmentRef { return p.rplSegments }

// TxParams seeds one write transaction. It replaces the former
// order-dependent setter sequence (SetCommitState → SetSizeParams →
// BeginTx → SetCurrentTxnID → three callback setters): one struct
// makes the seeding contract compiler-carried — a caller cannot
// half-seed the pager or misorder the seeding relative to the tx
// snapshot BeginTx takes.
type TxParams struct {
	// Committed baseline from the transaction's base meta.
	// HighWaterMark and MaxSize gate allocation/extension; GrowStep
	// and MinSize shape file growth/shrink (file-format.md §File
	// Growth / §File Shrinkage) — passed per tx so a SetFileFormat
	// commit is picked up on the next transaction.
	HighWaterMark uint64
	MaxSize       uint64
	GrowStep      uint64
	MinSize       uint64
	// TxnID is the transaction's id: stamps RPL segments at commit
	// (commit step 0) and feeds LaggingReaderInfo.Lag from the first
	// user-path AllocPage.
	TxnID uint64
	// ReclamationBound derives min(oldestActiveReaderTxnID,
	// lastCheckpointTxnID) (free-space.md §RPL Reclamation). BeginTx
	// calls it once to seed the bound; ReclaimFreeSpace and the
	// LaggingReaderWait retry path call it again to refresh. Any
	// refresh value is safe by construction: it is re-derived from
	// the live reader table so it never exceeds the true current
	// bound, reclamation frees only segments strictly below it, and
	// already-freed segments have left the in-memory chain, so a
	// transient dip (a reader registering in the
	// snapshot-read-to-slot-publish window can briefly lower the
	// scan) only delays reclamation — it cannot un-reclaim or
	// over-reclaim. nil ⇒ bound 0: nothing reclaims (fixtures/tests).
	ReclamationBound func() uint64
	// RPLCorrupt is invoked once per RPL segment quarantined during
	// reclamation (free-space.md §RPL Reclamation), with the
	// segment's page id. nil = no notification.
	RPLCorrupt func(segPageID uint64)
	// LaggingReader is consulted by AllocPage/AllocContiguous when
	// the bitmap is exhausted and reclamation is blocked by the
	// bound — at most once per call. nil = no callback; allocation
	// falls through to file extension.
	LaggingReader func(LaggingReaderInfo) LaggingReaderAction
}

// BeginTx seeds the pager for one write transaction from params, then
// snapshots the mutable state (bitmap, HighWaterMark, RPL chain) so
// AbortTx can restore it. Seeding precedes the snapshot: the snapshot
// must capture the seeded baseline (AbortTx restores HighWaterMark to
// it). Idempotent: a second call discards the first snapshot via
// bitmap.Discard before clobbering it (used by tests; production
// callers don't re-Begin without Commit/Rollback). The Discard is
// required because the bitmap tracks open Snapshots internally —
// silently overwriting bitmapSnapshot without releasing it would leak
// the prior Snapshot into openSnapshots and grow the undo log
// unbounded.
func (p *Pager) BeginTx(params TxParams) {
	// Reset the per-tx TxStats counters and the checksum-verification
	// cache unconditionally (read-only pagers included), matching the
	// pre-TxParams behavior; the seeding below is writer-only so
	// currentTxnID and the callbacks stay zero on read-only pagers.
	p.resetTxCounters()
	p.resetVerified()
	// Reset the per-tx commit reserve contributions.
	p.externalReserve = 0
	p.inCommit = false
	if p.readOnly {
		return
	}
	// Install the seeding before the tx snapshot (see doc above).
	p.highWaterMark = params.HighWaterMark
	p.maxSizePages = params.MaxSize
	p.growStepPages = params.GrowStep
	p.minSizePages = params.MinSize
	p.currentTxnID = params.TxnID
	p.refreshReclamationBound = params.ReclamationBound
	p.rplCorruptCb = params.RPLCorrupt
	p.laggingReader = params.LaggingReader
	if params.ReclamationBound != nil {
		p.reclamationBound = params.ReclamationBound()
	} else {
		p.reclamationBound = 0
	}
	if p.bitmap == nil {
		return
	}
	if p.haveTxSnapshot {
		p.bitmap.Discard(p.bitmapSnapshot)
	}
	p.bitmapSnapshot = p.bitmap.Snapshot()
	p.hwmSnapshot = p.highWaterMark
	p.rplChainSnapshot = slices.Clone(p.rplSegments)
	p.haveTxSnapshot = true
}

// AbortTx restores the snapshotted state, releases slab buffers, and
// resets tx-scoped freespace bookkeeping. The committed on-disk state is
// unaffected: slab pages are pwritten only by the commit pipeline (so a
// rollback before commit has none), and WriteDirect's slab-bypass pages
// (BulkLoad) — which ARE pwritten mid-transaction — revert to free when
// the bitmap snapshot is restored here. The on-disk bitmap is pwritten
// only at commit, so it never recorded those allocations; their pwritten
// bytes are harmless stale content in pages the allocator now sees as
// free and will overwrite on reuse. A clean rollback therefore reclaims
// every bulk-written page with no leak (no maintenance pass needed).
//
// After AbortTx returns, the pager is ready to start a fresh
// transaction via BeginTx(TxParams).
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
	// TxStats: a rolled-back tx's slab peak is not representative of
	// steady-state need (api-surface.md §Statistics), so a post-rollback
	// Stats() reports SlabPeakBytes = 0. The other counters persist to
	// reflect the (rolled-back) activity until the next BeginTx resets.
	p.zeroSlabPeak()
	p.ReleaseAll()
	clear(p.pendingAllocs)
	clear(p.pendingFrees)
	clear(p.loosePages)
	p.retiredPages = p.retiredPages[:0]
	p.currentTxnID = 0
	// A top-level abort supersedes any open child savepoints. The root
	// package's Rollback cascade resolves the open descendant chain
	// (draining the savepoint stack) before aborting, so this is
	// defense-in-depth — but resetting keeps AllocPage's loose-pop
	// guard correct if a future caller path ever aborts with a
	// dangling savepoint. The savepointUndoLog reset
	// is required for the same reason: any leaked entries would
	// otherwise survive the tx boundary and a later Begin's undoLogPos
	// would index past stale entries.
	p.savepointDepth = 0
	p.activeSavepoints = p.activeSavepoints[:0]
	p.savepointUndoLog = p.savepointUndoLog[:0]
}

// discardTxSnapshot drops the snapshot without restoring (Commit
// success path). Releases the bitmap's per-Snapshot undo-log tracking
// via bitmap.Discard: with no Snapshot left open the bitmap truncates
// its undo log to length 0, so the log never survives across a tx
// boundary. Guard mirrors AbortTx's haveTxSnapshot && bitmap != nil
// pattern.
func (p *Pager) discardTxSnapshot() {
	if p.haveTxSnapshot && p.bitmap != nil {
		p.bitmap.Discard(p.bitmapSnapshot)
	}
	p.bitmapSnapshot = nil
	p.rplChainSnapshot = nil
	p.haveTxSnapshot = false
	// Defense-in-depth, symmetric with AbortTx: a leaked (un-Released,
	// un-Restored) savepoint of either kind would otherwise survive past
	// the commit boundary, and a subsequent loose-pop in the next tx
	// would append to its stale loosePopLog (Shallow) or its dangling
	// undoLogPos would index against the next tx's log entries. All
	// production callers (the per-row indexed maintenance helpers and
	// the DDL nested-savepoint sites) are leak-free by construction,
	// so this is belt-and-braces — but matches AbortTx's reset pattern.
	p.activeSavepoints = p.activeSavepoints[:0]
	p.savepointUndoLog = p.savepointUndoLog[:0]
}

// HighWaterMark returns the current write-tx HighWaterMark snapshot.
func (p *Pager) HighWaterMark() uint64 { return p.highWaterMark }

// NumFreePages returns the current count of bitmap-free data pages (pages
// available to AllocPage without reclamation or file extension). Used by
// incremental compaction to size its evacuation band against the free space
// actually reachable this transaction (after ReclaimFreeSpace).
func (p *Pager) NumFreePages() uint64 {
	if p.bitmap == nil {
		return 0
	}
	return p.bitmap.NumFree()
}

// PendingAllocs / PendingFrees / RetiredPages expose the tx-scoped
// bookkeeping for the commit-step-0 caller. Returned
// slices/maps are owned by the pager — callers must not mutate.
func (p *Pager) PendingAllocs() map[uint64]struct{} { return p.pendingAllocs }
func (p *Pager) PendingFrees() map[uint64]struct{}  { return p.pendingFrees }
func (p *Pager) RetiredPages() []uint64             { return p.retiredPages }
func (p *Pager) LoosePages() map[uint64]struct{}    { return p.loosePages }

// RPLPageCount returns the number of retired data-page entries pinned in
// the committed RPL chain (the sum of each segment's entry count) —
// pages freed by prior transactions that reclamation has not yet handed
// back, gated by the oldest live reader / last checkpoint. Used by
// DBStats.RetiredPages. Zero on a read-only pager (no RPL chain loaded).
// Single-threaded pager contract: callers read it from the writer
// goroutine or while no write transaction is mutating the chain.
func (p *Pager) RPLPageCount() uint64 {
	var n uint64
	for _, seg := range p.rplSegments {
		n += uint64(seg.Count)
	}
	return n
}

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

// RPLCorruptCount returns the number of RPL segments quarantined this
// session due to a decode failure during reclamation (free-space.md
// §RPL Reclamation). Non-zero means reclamation skipped a corrupt
// segment: its pages leak (bounded to that segment) until a Check()
// /Repair structural walk reclaims them.
func (p *Pager) RPLCorruptCount() uint64 { return p.rplCorruptCount }

// ReclaimFreeSpace eagerly reclaims every RPL segment now below the
// reclamation bound, returning their pages to the allocation bitmap, and
// returns the number of pages reclaimed. It first refreshes the bound
// via TxParams.ReclamationBound when one was supplied (production
// always supplies it; any refresh value is safe by construction — see
// TxParams.ReclamationBound); without one it uses the seeded bound,
// the same gate AllocPage's lazy reclaim uses. Normally reclamation is lazy —
// AllocPage triggers it only when the bitmap is exhausted. Incremental
// compaction calls this at the start of its transaction so that (a) already-
// reclaimable free space is available to relocate *into* before the first
// AllocPage (avoiding a needless file extension), and (b) the previous pass's
// relocated-from pages — now committed under an older TxnID than the bound —
// are returned to the bitmap promptly, so a trailing run of them tail-refunds
// at this commit rather than waiting for a future bitmap-exhaustion. It is
// reader-safe: reclaimRPL only frees segments strictly below the bound, the
// same gate AllocPage uses. Must be called on the writer pager within a
// transaction (after BeginTx).
func (p *Pager) ReclaimFreeSpace() int {
	if p.refreshReclamationBound != nil {
		p.reclamationBound = p.refreshReclamationBound()
	}
	return p.reclaimRPL()
}

// FileSize returns the file size observed at Open. Used to bound reads
// (callers must additionally respect HighWaterMark from the active meta).
func (p *Pager) FileSize() int64 { return p.fileSize }

// DirtyBytes returns the current slab usage in bytes (writable pagers
// only; returns 0 on read-only pagers).
func (p *Pager) DirtyBytes() int { return p.dirtyBytes }

// MaxBytes returns the slab budget (writable pagers only).
func (p *Pager) MaxBytes() int { return p.maxBytes }

// rplReserveBytes is the exact slab cost of the RPL segment pages
// commit step 0 will allocate for the pages retired so far:
// ceil(len(retiredPages) / RPLEntriesPerSegment) segment pages. The
// projection is live — savepoint rollback truncates retiredPages, so
// the reserve shrinks with it — and exact, because appendRPL's
// AllocSlab calls are the only slab allocations Commit performs
// (descriptor-flush writes are pre-paid during the ops phase per
// transactions.md §Commit Pipeline, bitmap pages pwrite from the
// bitmap's own storage, and the meta page is a non-slab scratch
// buffer).
func (p *Pager) rplReserveBytes() int {
	n := len(p.retiredPages)
	if n == 0 {
		return 0
	}
	capPerSeg := page.RPLEntriesPerSegment(p.cfg)
	if capPerSeg <= 0 {
		// Commit will reject this geometry loudly (appendRPL errors);
		// no reserve can make it committable.
		return 0
	}
	segs := (n + capPerSeg - 1) / capPerSeg
	return segs * int(p.cfg.PageSize)
}

// SetExternalReserve installs the caller-maintained portion of the
// commit reserve — the db layer's projection of the slab cost of its
// commit-time descriptor flush (Tx.recalcFlushReserve). The pager
// treats it opaquely: ops-phase admission subtracts it alongside the
// internal RPL projection, and the commit phase draws from it.
func (p *Pager) SetExternalReserve(n int) {
	if n < 0 {
		n = 0
	}
	p.externalReserve = n
}

// SetCommitPhase toggles commit-phase admission (see the inCommit
// field). The db layer sets it around its commit-time descriptor
// flush, which runs before Commit; Commit manages the flag itself
// for its own steps.
func (p *Pager) SetCommitPhase(on bool) { p.inCommit = on }

// CommitReserveBytes exposes the live commit-time reserve (internal
// RPL projection + external descriptor-flush projection) — the gap
// between MaxBytes and the ops-phase admission limit. Used by
// budget-edge tests to compute effective headroom.
func (p *Pager) CommitReserveBytes() int { return p.rplReserveBytes() + p.externalReserve }

// admissionLimit is the effective slab budget for ops-phase
// allocations: maxBytes minus the commit-time reserve, so a
// transaction the admission accepts can always afford its own commit
// (pager-slab.md §Slab Budget). During the commit phase the reserve
// is released — commit's allocations are the reserved pages.
func (p *Pager) admissionLimit() int {
	if p.inCommit {
		return p.maxBytes
	}
	return p.maxBytes - p.rplReserveBytes() - p.externalReserve
}

// IsReadOnly reports whether mutating operations are rejected.
func (p *Pager) IsReadOnly() bool { return p.readOnly }

// pageRaw returns a borrowed byte slice for the page at id WITHOUT
// file-resident bounds-checking or checksum verification. Resolution
// order: dirty[id] (write txn only) then mmap.
//
// The returned slice has length cfg.PageSize and is valid as long as the
// transaction owns the pager (the slab buffer survives loose-page
// retirement until Commit/Rollback per the byte-slice ownership
// invariant in pager-slab.md). For mmap-backed reads the slice is
// valid until the mmap is unmapped (i.e. Close()).
//
// Panics if id*PageSize would exceed the mmap reservation. This is the
// low-level accessor: callers that read content-derived ids from a
// possibly-corrupt tree MUST go through the verifying Page (which
// bounds against the file-resident extent before any mmap access);
// pageRaw is for callers that have already bounded id (the btree Walk /
// WalkKV hwm gate, Open-time scans bounded by their own cycle caps,
// and Check's deliberately-unverified reporting reads).
func (p *Pager) pageRaw(id uint64) []byte {
	if !p.readOnly {
		if buf, ok := p.dirty[id]; ok {
			return *buf
		}
	}
	off := id * uint64(p.cfg.PageSize)
	end := off + uint64(p.cfg.PageSize)
	if end > uint64(len(p.mmap)) {
		panic(fmt.Sprintf("pager: pageRaw(%d) past mmap reservation [%d, %d]", id, off, end))
	}
	if p.trackCold {
		p.recordAccess(id)
	}
	return p.mmap[off:end]
}

// PageRaw exposes pageRaw to other packages that have already bounded
// id and deliberately want unverified bytes (the gmdb Check path, which
// reports corruption as issues rather than aborting on it). It panics
// past the mmap reservation exactly like pageRaw.
func (p *Pager) PageRaw(id uint64) []byte { return p.pageRaw(id) }

// AdviseHugePages hints transparent-huge-page backing on the whole data
// mmap (mmap-strategy.md §Huge Pages, Options.HugePages). Silent no-op
// on non-Linux and on kernels without THP for file-backed mappings.
func (p *Pager) AdviseHugePages() error { return madviseHugePage(p.mmap) }

// AdvisePreload prefaults pages [0, throughPage) into the OS page cache
// via MADV_POPULATE_READ (mmap-strategy.md §Prefaulting,
// Options.PreloadPages / CopyTo). throughPage is the snapshot's
// HighWaterMark — the file-backed extent. Silent no-op on non-Linux and
// on kernels < 5.14.
func (p *Pager) AdvisePreload(throughPage uint64) error {
	length := int(throughPage * uint64(p.cfg.PageSize))
	return madvisePopulateRead(p.mmap, length)
}

// EnableColdTracking turns on per-transaction accessed-page tracking for
// Options.ReclaimOnClose (mmap-strategy.md §Read Transaction Cooldown).
// Must be called before the first page read (the read-tx Begin path), on
// a read-only pager only. Page / pageRaw then record the min/max page
// IDs touched; AdviseColdAccessed issues the MADV_COLD on close.
func (p *Pager) EnableColdTracking() {
	p.accessMin.Store(^uint64(0)) // MaxUint64: first access wins the min CAS
	p.accessMax.Store(0)
	p.trackCold = true
}

// recordAccess folds page id into the [accessMin, accessMax] range via
// two atomic CAS loops — the "two atomic min/max updates per page read"
// of mmap-strategy.md §Read Transaction Cooldown. Only reached when
// trackCold is set.
func (p *Pager) recordAccess(id uint64) {
	for {
		cur := p.accessMin.Load()
		if id >= cur {
			break
		}
		if p.accessMin.CompareAndSwap(cur, id) {
			break
		}
	}
	for {
		cur := p.accessMax.Load()
		if id <= cur {
			break
		}
		if p.accessMax.CompareAndSwap(cur, id) {
			break
		}
	}
}

// AdviseColdAccessed issues a single MADV_COLD over the page range this
// transaction touched (mmap-strategy.md §Read Transaction Cooldown),
// hinting the kernel it may reclaim those pages under memory pressure.
// No-op when cold-tracking is off or no page was accessed (min > max).
// Called by the read-tx close path BEFORE the mmap is unmapped. Silent
// no-op on non-Linux and on kernels < 5.4.
func (p *Pager) AdviseColdAccessed() error {
	if !p.trackCold {
		return nil
	}
	minID := p.accessMin.Load()
	maxID := p.accessMax.Load()
	if minID > maxID {
		return nil // no page accessed this transaction
	}
	off := int(minID * uint64(p.cfg.PageSize))
	length := int((maxID - minID + 1) * uint64(p.cfg.PageSize))
	return madviseCold(p.mmap, off, length)
}

// Page resolves the page at id for the btree read/write paths, returning
// an error rather than panicking or returning unverified bytes. It is
// the boundary that enforces the read-path corruption-tolerance
// invariants (checksums.md §Verification):
//
//   - File-resident bound (checksums.md §Structural and Allocation Bounds): a content-derived id is bounded against the file-resident
//     extent before any mmap access, so a forged/out-of-range id yields
//     ErrCorrupted instead of a SIGBUS on the unbacked MaxSize
//     reservation. (A dirty slab buffer lives in process memory and is
//     this-tx content, so it is returned without bound or verification.)
//   - Checksum verification (checksums.md §Verification): when checksums are enabled, the xxhash64 footer is
//     verified on the page's first mmap access within the transaction
//     and the id recorded in p.verified so subsequent accesses skip the
//     re-hash; a mismatch yields ErrBadPageChecksum.
func (p *Pager) Page(id uint64) ([]byte, error) {
	if !p.readOnly {
		if buf, ok := p.dirty[id]; ok {
			return *buf, nil
		}
	}
	// File-resident bound (checksums.md §Structural and Allocation Bounds): bound id against the file-resident page count BEFORE any
	// mmap access. The mmap spans the whole MaxSize reservation, but only
	// the first p.fileSize bytes are file-backed — reading the gap
	// SIGBUSes. Comparing page counts (not byte offsets) keeps id*PageSize
	// from overflowing uint64 for a forged-huge id.
	backedPages := uint64(p.fileSize) / uint64(p.cfg.PageSize)
	if id >= backedPages {
		return nil, fmt.Errorf("%w: page id %d beyond file-resident extent (%d pages)",
			ErrCorrupted, id, backedPages)
	}
	if p.trackCold {
		p.recordAccess(id)
	}
	off := id * uint64(p.cfg.PageSize)
	buf := p.mmap[off : off+uint64(p.cfg.PageSize)]
	// Checksum verification (checksums.md §Verification): verify the footer on first access this tx, then cache.
	if p.cfg.PageChecksum && !p.isVerified(id) {
		if !page.VerifyPageFooter(buf, p.cfg.PageSize) {
			return nil, fmt.Errorf("%w: page %d", ErrBadPageChecksum, id)
		}
		p.markVerified(id, backedPages)
	}
	return buf, nil
}

// isVerified reports whether page id's footer has already been verified
// in this transaction (checksums.md §Verification). Out-of-range ids (never cached) return
// false; the file-resident bound in Page rejects them before this is
// reached, so the guard is purely defensive.
func (p *Pager) isVerified(id uint64) bool {
	w := id >> 6
	if w >= uint64(len(p.verified)) {
		return false
	}
	return p.verified[w]&(1<<(id&63)) != 0
}

// markVerified records page id as verified for the rest of this tx. The
// bitset is lazily sized to cover the file-resident extent on first use;
// a page id is always < backedPages here (Page bounds it first).
func (p *Pager) markVerified(id, backedPages uint64) {
	if p.verified == nil {
		p.verified = make([]uint64, (backedPages+63)>>6)
	}
	w := id >> 6
	if w >= uint64(len(p.verified)) {
		// File grew since the bitset was sized (write tx extension).
		// Grow to cover id; the freshly-zeroed words mark nothing
		// verified, which is correct (those pages were never read yet).
		grown := make([]uint64, w+1)
		copy(grown, p.verified)
		p.verified = grown
	}
	p.verified[w] |= 1 << (id & 63)
}

// resetVerified clears the per-tx checksum-verification cache. Called at
// each write-tx boundary because committed mmap content can change
// across transactions (a page rewritten by the previous commit must be
// re-verified). Read pagers never reuse, so they never call this.
func (p *Pager) resetVerified() { p.verified = nil }

// CoW installs a fresh slab buffer at dstID populated from the current
// content of srcID. dstID is supplied by the caller's allocator (see
// free-space.md §Page Allocation Priority); srcID
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
	if p.dirtyBytes+int(p.cfg.PageSize) > p.admissionLimit() {
		return nil, ErrTxTooLarge
	}
	src := p.pageRaw(srcID)
	buf := p.bufPool.Get()
	copy(*buf, src)
	p.dirty[dstID] = buf
	p.dirtyBytes += int(p.cfg.PageSize)
	p.tc.cow++
	p.bumpSlabPeak()
	// Reaching this branch means dirty[dstID] was absent (the
	// idempotent shortcut above returned otherwise), so the dirty-add
	// undo entry has wasPresent=false. RestoreSavepoint deletes
	// dirty[dstID] and pool-Puts the buffer.
	p.recordSavepointUndo(fieldDirty, dstID, false)
	return *buf, nil
}

// AllocSlab installs a fresh zero-filled slab buffer at id without
// reading any source page. Used by commit step 0 to assemble RPL
// segment pages and by tx-body writers building pages with no prior
// on-disk content. Returns ErrTxTooLarge on budget overrun.
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
	if p.dirtyBytes+int(p.cfg.PageSize) > p.admissionLimit() {
		return nil, ErrTxTooLarge
	}
	buf := p.bufPool.Get()
	p.dirty[id] = buf
	p.dirtyBytes += int(p.cfg.PageSize)
	p.bumpSlabPeak()
	p.recordSavepointUndo(fieldDirty, id, false)
	return *buf, nil
}

// AllocSlabRun installs n fresh zero-filled slab buffers covering the
// contiguous run [firstID, firstID+n) previously reserved via
// AllocContiguous. pages[i] is the buffer for firstID + uint64(i).
// Implements the PageWriter contract used by the
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
	if int64(p.dirtyBytes)+fresh*int64(p.cfg.PageSize) > int64(p.admissionLimit()) {
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
		p.recordSavepointUndo(fieldDirty, id, false)
		out[i] = *buf
	}
	p.bumpSlabPeak()
	return out, nil
}

// WriteDirect pwrites a fully-formed page buffer to disk at id's file
// offset, bypassing the slab entirely. It is the bulk-load escape hatch
// from pager-slab.md §Slab Budget / bulkload.md §Slab Bypass: pages
// constructed bottom-up are pwritten as they are completed so memory
// stays O(depth × pageSize) regardless of input size, never charging the
// MaxTxBufferBytes budget.
//
// Contract (the WriteDirect invariant, bulkload.md §Slab Bypass):
//   - id MUST have been allocated in this transaction (present in
//     pendingAllocs). This reserves id's bitmap bit (cleared = in-use)
//     so no other allocation in the tx reuses it, and guarantees the
//     file already covers id (AllocPage's file-extension path ftruncated
//     up). Writing to a page not reserved by this tx could clobber a
//     page reachable from the active meta — corruption.
//   - id MUST NOT be in the slab (p.dirty). A slab buffer at the same id
//     would be re-pwritten by commitStep1 over this direct content (map
//     iteration order is undefined), so the committed tree could
//     reference a page with the wrong bytes. The two write paths are
//     mutually exclusive per page.
//
// On PageChecksum, WriteDirect writes the xxhash64 footer into the last
// FooterSize bytes of buf in place — identical to commitStep1 — before
// the pwrite, so a subsequent checksum-verified read of id succeeds.
// The caller may reuse buf after WriteDirect returns.
//
// Durability: the direct pwrite lands in the OS page cache immediately.
// It is made durable by the same step-2 fdatasync(fd) that the commit
// protocol issues over the whole file before the meta swap — the direct
// pages are flushed alongside the slab pages. Until the meta swap
// publishes the new keyspace root, the direct pages are at fresh IDs
// unreferenced by any recoverable meta (bulkload.md §Atomicity). On
// abort, AbortTx restores the bitmap/HWM snapshot, so id reverts to free
// on the in-memory bitmap and the on-disk bitmap (never pwritten
// pre-commit) still marks it free — the direct content is harmless stale
// bytes in a free page.
//
// Errors: ErrReadOnly on a read-only pager; ErrFreespaceUnconfigured if
// the bitmap is unattached; a wrapped sentinel-free error for a buf whose
// length is not exactly PageSize, an id not in pendingAllocs, or an id
// already in the slab.
func (p *Pager) WriteDirect(id uint64, buf []byte) error {
	if p.readOnly {
		return ErrReadOnly
	}
	if p.bitmap == nil {
		return ErrFreespaceUnconfigured
	}
	if len(buf) != int(p.cfg.PageSize) {
		return fmt.Errorf("pager: WriteDirect buf len %d != PageSize %d", len(buf), p.cfg.PageSize)
	}
	if _, ok := p.pendingAllocs[id]; !ok {
		return fmt.Errorf("pager: WriteDirect page %d not allocated in this transaction", id)
	}
	if _, ok := p.dirty[id]; ok {
		return fmt.Errorf("pager: WriteDirect page %d also in slab (dirty) — paths are mutually exclusive: %w", id, ErrCorrupted)
	}
	// Redundant safety assertion at the persistence boundary: the
	// pendingAllocs guard above already guarantees the file covers id
	// (every allocation path ftruncates up before returning the id, and
	// HWM only advances through ensureFileCovers). ensureFileCovers
	// short-circuits to a no-op when fileSize already covers id+1; it is
	// kept so a future allocation path that forgets to extend cannot
	// turn into a silent sparse-hole pwrite past EOF.
	if err := p.ensureFileCovers(id + 1); err != nil {
		return fmt.Errorf("pager: WriteDirect ensure file covers page %d: %w", id, err)
	}
	if p.cfg.PageChecksum {
		page.WritePageFooter(buf, p.cfg.PageSize)
	}
	off := int64(id) * int64(p.cfg.PageSize)
	if _, err := p.file.WriteAt(buf, off); err != nil {
		return fmt.Errorf("pager: WriteDirect pwrite page %d: %w", id, err)
	}
	return nil
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
// []byte until tx close. The freespace state machine
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
// AllocPage loose-pop path — see the comment on
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

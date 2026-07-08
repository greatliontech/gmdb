package pager

import (
	"errors"
	"fmt"
	"io"
	"os"
	"slices"

	"github.com/thegrumpylion/gmdb/internal/bitmap"
	"github.com/thegrumpylion/gmdb/internal/page"
)

// InitParams describes a fresh database file's creation parameters. All
// fields are caller-supplied; the pager applies no defaults.
type InitParams struct {
	PageSize        uint32
	PageChecksum    bool
	MinSize         uint64 // minimum file size in pages
	MaxSize         uint64 // maximum file size in pages (immutable)
	GrowStep        uint64 // file growth step in pages
	ShrinkThreshold uint64 // shrink threshold in pages
	UUID            [16]byte
}

// BitmapPages returns the number of bitmap pages required to cover
// MaxSize at the configured PageSize:
//
//	ceil(MaxSize / (PageSize * 8))
//
// Used both by Init (to write the meta) and by Open (to size the
// in-memory bitmap detail when reading the on-disk region).
func (ip InitParams) BitmapPages() uint32 {
	bitsPerPage := uint64(ip.PageSize) * 8
	return uint32((ip.MaxSize + bitsPerPage - 1) / bitsPerPage)
}

// Init creates a fresh database in file: writes two valid meta pages,
// the bitmap region (all zeros — no free pages, no data pages allocated),
// and truncates the file to MinSize pages (or to the meta + bitmap
// region, whichever is larger).
//
// Both meta pages are written identically at TxnID = 0; the active-meta
// selector picks meta 0 on Open via the file-layout.md tie-break rule
// for TxnID-zero ties.
func Init(file *os.File, ip InitParams) error {
	if !page.ValidPageSize(ip.PageSize) {
		return fmt.Errorf("pager: invalid PageSize %d", ip.PageSize)
	}
	if ip.MaxSize == 0 {
		return fmt.Errorf("pager: MaxSize must be > 0")
	}
	if ip.MinSize > ip.MaxSize {
		return fmt.Errorf("pager: MinSize %d > MaxSize %d", ip.MinSize, ip.MaxSize)
	}
	bitmapPages := ip.BitmapPages()
	firstDataPage := uint64(2) + uint64(bitmapPages)

	// Sized in pages: the minimum file is meta(2) + bitmap region.
	// If MinSize is below that, extend; otherwise honour the caller.
	floorPages := firstDataPage
	filePages := ip.MinSize
	if filePages < floorPages {
		filePages = floorPages
	}

	flags := uint32(0)
	if ip.PageChecksum {
		flags |= MetaFlagPageChecksum
	}

	m := Meta{
		Magic:           page.Magic,
		Version:         page.FormatVersion,
		PageSize:        ip.PageSize,
		Flags:           flags,
		BitmapPages:     bitmapPages,
		UUID:            ip.UUID,
		MinSize:         ip.MinSize,
		MaxSize:         ip.MaxSize,
		GrowStep:        ip.GrowStep,
		ShrinkThreshold: ip.ShrinkThreshold,
		HighWaterMark:   firstDataPage,
		RPLHeadPage:     0,
		RPLHeadTxnID:    0,
		RPLTailPage:     0,
		RPLEntryCount:   0,
		NumFreePages:    0,
		KeyspaceRoot:    0,
		NumKeyspaces:    0,
		TxnID:           0,
	}
	// Genesis metas are self-durable at epoch 0 (api-surface.md
	// §Database Initialization): the fsync below makes the empty
	// state the first durable epoch. AnchoredTxnID = 0 is harmless
	// even if a crash beats the fsync — a bound of 0 reclaims
	// nothing.
	m.Durable = m.LiveSubRecord()
	m.Durable.AnchoredTxnID = 0

	pageSizeI := int64(ip.PageSize)
	if err := file.Truncate(int64(filePages) * pageSizeI); err != nil {
		return fmt.Errorf("pager: truncate: %w", err)
	}

	// Write meta page 0 and meta page 1. Each occupies a full page; the
	// meta payload is the first MetaPayloadSize bytes and the rest of
	// the page is zero-filled.
	metaBuf := make([]byte, ip.PageSize)
	EncodeMeta(metaBuf, &m)
	for slot := 0; slot < 2; slot++ {
		off := int64(slot) * pageSizeI
		if _, err := file.WriteAt(metaBuf, off); err != nil {
			return fmt.Errorf("pager: write meta %d: %w", slot, err)
		}
	}

	// Bitmap region is already zero from the truncate. Nothing to write.

	if err := file.Sync(); err != nil {
		return fmt.Errorf("pager: fsync init: %w", err)
	}
	return nil
}

// OpenParams supplies the runtime configuration Open needs. Persisted
// fields (PageSize, PageChecksum, MaxSize, ...) come from the meta page
// itself.
type OpenParams struct {
	Pool             *BufPool // page-sized buffer pool; PageSize must match
	MaxTxBufferBytes int      // slab budget for write transactions
}

// OpenedDB bundles the products of Open: the UNATTACHED writer pager,
// the selected meta, and its slot index. Meta/ActiveMetaIdx are the
// pre-grant snapshot — enough for cfg/UUID needs — and are superseded
// by the result of the attach call (RecoverToDurable or AttachLatest,
// see Open) that the caller MUST make under the write grant before
// using the pager. The caller advances PrevActive on commit and
// re-snapshots Meta from the post-commit return.
type OpenedDB struct {
	Pager         *Pager
	Meta          Meta
	ActiveMetaIdx int
}

// Open reads the file's two meta pages, selects the active one,
// validates its fields, and mmaps the data file with a reservation of
// `Meta.MaxSize * PageSize`. The returned writer pager is UNATTACHED —
// no in-memory bitmap, RPL chain, or commit state; see the body
// comment and OpenedDB for the mandatory second phase.
//
// The returned Pager's pool is op.Pool; it must outlive the pager.
//
// Meta-0 corruption recovery: when meta-0 fails its checksum verify
// (whether the PageSize bytes are invalid, or any other field has been
// tampered), the PageSize is rediscovered by probing each supported
// page size against meta-1's offset. The file-layout.md dual-meta
// atomicity invariant guarantees recoverability if at least one meta
// has a passing checksum at its correct offset; the probe is the
// probe mechanism that honours it.
func Open(file *os.File, op OpenParams) (*OpenedDB, error) {
	if op.Pool == nil {
		return nil, fmt.Errorf("pager: Pool must not be nil")
	}
	if op.MaxTxBufferBytes <= 0 {
		return nil, fmt.Errorf("pager: MaxTxBufferBytes must be > 0")
	}
	// 1–2) Discover PageSize, select + validate the active meta. Shared
	// with OpenReadOnly (which then builds a reader pager instead).
	m, active, pageSize, err := readAndSelectMeta(file)
	if err != nil {
		return nil, err
	}

	cfg := page.Config{PageSize: pageSize, PageChecksum: m.HasFlag(MetaFlagPageChecksum)}

	// 3) Reservation = MaxSize * PageSize, mmap, mprotect.
	reservation := int64(m.MaxSize) * int64(pageSize)
	p, err := NewWriter(file, cfg, reservation, op.Pool, op.MaxTxBufferBytes)
	if err != nil {
		return nil, err
	}

	// The writer pager is returned UNATTACHED: building the in-memory
	// bitmap + RPL chain walks the selected meta's projection, and
	// WHICH projection (live vs durable) is the recovery-commit gate's
	// decision, which only the root package can take — under the write
	// grant, against a grant-current re-read of the metas
	// (durability.md §Recovery step 5). Walking the live projection
	// here would also hard-fail on a crashed SyncLazy image whose
	// unflushed post-epoch RPL head is exempt from boundary treatment
	// — permanently failing an Open the durable projection recovers.
	// The caller MUST call exactly one of RecoverToDurable (gate
	// passed) or AttachLatest (live join / self-durable-no-gate) under
	// the grant before using the pager; Meta/ActiveMetaIdx returned
	// here are the pre-grant snapshot for cfg/UUID needs only and are
	// superseded by that call's result.
	return &OpenedDB{
		Pager:         p,
		Meta:          m,
		ActiveMetaIdx: active,
	}, nil
}

// AttachLatest re-reads both meta slots under the caller's write
// grant, selects the latest valid meta (the one selection), attaches
// the pager to its LIVE projection, and returns it. The under-grant
// re-read is load-bearing: the pre-grant Open snapshot can be stale by
// any number of peer commits that landed while AcquireWriter blocked.
func (p *Pager) AttachLatest(file *os.File) (Meta, int, error) {
	meta0, meta1, err := readMetaPair(file, p.cfg.PageSize)
	if err != nil {
		return Meta{}, 0, err
	}
	active, ok := ActiveMeta(meta0, meta1)
	if !ok {
		return Meta{}, 0, errBothMetasInvalid()
	}
	m, err := decodeActiveMeta(meta0, meta1, active)
	if err != nil {
		return Meta{}, 0, err
	}
	if err := p.attachState(file, m); err != nil {
		return Meta{}, 0, err
	}
	return m, active, nil
}

// OpenReadOnly opens a read-only pager over file for a read-only DB
// handle (Options.ReadOnly). It performs the SAME meta discovery,
// active-meta selection, and validation as Open (readAndSelectMeta),
// but builds a NewReader pager — no writer slab, no in-memory
// bitmap/RPL, no attachState — because a read-only handle never
// allocates, frees, or commits. op.Pool / op.MaxTxBufferBytes are not
// consulted (no writer slab is built).
//
// The returned pager owns the data mmap (PROT_READ) for the handle's
// lifetime and backs handle-level raw reads (e.g. Check); each read
// transaction still brings up its own per-snapshot reader pager (the
// root package's BeginRead). file may be opened O_RDONLY — nothing in
// this path writes it.
func OpenReadOnly(file *os.File, op OpenParams) (*OpenedDB, error) {
	m, active, pageSize, err := readAndSelectMeta(file)
	if err != nil {
		return nil, err
	}
	cfg := page.Config{PageSize: pageSize, PageChecksum: m.HasFlag(MetaFlagPageChecksum)}
	reservation := int64(m.MaxSize) * int64(pageSize)
	p, err := NewReader(file, cfg, reservation)
	if err != nil {
		return nil, err
	}
	return &OpenedDB{
		Pager:         p,
		Meta:          m,
		ActiveMetaIdx: active,
	}, nil
}

// readAndSelectMeta reads the two meta-page images, discovers the
// PageSize (trusting meta-0's only when its checksum verifies,
// otherwise probing meta-1 candidate offsets — a passing checksum is
// the only thing that authorizes trust in any meta field, since
// ValidPageSize alone can't catch a checksum-breaking flip that still
// yields a syntactically valid value), and selects + validates the
// active meta — the ONE selection every consumer uses (durability.md
// §One selection, two projections; crash recovery differs only by
// adopting the winner's durable sub-record, which is the CALLER's
// step). Shared by Open (then NewWriter + attachState) and
// OpenReadOnly (then NewReader).
func readAndSelectMeta(file *os.File) (m Meta, active int, pageSize uint32, err error) {
	meta0Bytes := make([]byte, MetaPayloadSize)
	if _, err := file.ReadAt(meta0Bytes, 0); err != nil {
		return Meta{}, 0, 0, fmt.Errorf("pager: read meta0: %w", err)
	}
	// An intact gmdb meta-0 of a different format version is reported
	// distinctly from corruption (file-layout.md §Meta Page): the file
	// is fine, this binary just can't read its format. Checked before
	// the recovery machinery so a different-version file never
	// masquerades as a torn/corrupt current-version file.
	if isVersionMismatchMeta(meta0Bytes) {
		return Meta{}, 0, 0, fmt.Errorf("pager: %w: meta0 version %d, want %d",
			ErrVersionMismatch, DecodeMeta(meta0Bytes).Version, page.FormatVersion)
	}
	var meta1Bytes []byte
	if isGmdbMeta(meta0Bytes) {
		pageSize = DecodeMeta(meta0Bytes).PageSize
		if !page.ValidPageSize(pageSize) {
			// Checksum agrees with a value that the format rejects:
			// the file was written by a different format version or
			// the checksum collided. Either way, ErrCorrupted.
			return Meta{}, 0, 0, fmt.Errorf("pager: meta0 verified but PageSize %d invalid: %w", pageSize, ErrCorrupted)
		}
	} else {
		var perr error
		pageSize, meta1Bytes, perr = probeMetaPageSize(file)
		if perr != nil {
			return Meta{}, 0, 0, fmt.Errorf("pager: meta1 probe read: %w", perr)
		}
		if pageSize == 0 {
			return Meta{}, 0, 0, fmt.Errorf("pager: meta0 verify failed and meta1 probe found no recoverable meta: %w", ErrCorrupted)
		}
	}
	if meta1Bytes == nil {
		meta1Bytes = make([]byte, MetaPayloadSize)
		if _, err := file.ReadAt(meta1Bytes, int64(pageSize)); err != nil {
			return Meta{}, 0, 0, fmt.Errorf("pager: read meta1: %w", err)
		}
	}
	active, ok := ActiveMeta(meta0Bytes, meta1Bytes)
	if !ok {
		return Meta{}, 0, 0, errBothMetasInvalid()
	}
	m, err = decodeActiveMeta(meta0Bytes, meta1Bytes, active)
	if err != nil {
		return Meta{}, 0, 0, err
	}
	return m, active, pageSize, nil
}

// readMetaPair reads the two meta-page images at their fixed offsets
// (0 and pageSize). pageSize must already be trusted (from a verified
// meta or the immutable pager config) — page-size DISCOVERY on an
// unknown file is readAndSelectMeta's probe, not this helper.
func readMetaPair(file *os.File, pageSize uint32) (meta0, meta1 []byte, err error) {
	meta0 = make([]byte, MetaPayloadSize)
	if _, err := file.ReadAt(meta0, 0); err != nil {
		return nil, nil, fmt.Errorf("pager: read meta0: %w", err)
	}
	meta1 = make([]byte, MetaPayloadSize)
	if _, err := file.ReadAt(meta1, int64(pageSize)); err != nil {
		return nil, nil, fmt.Errorf("pager: read meta1: %w", err)
	}
	return meta0, meta1, nil
}

// decodeActiveMeta decodes the selected slot of a meta pair and
// validates it. Every selection path funnels through this one
// decode+validate.
func decodeActiveMeta(meta0, meta1 []byte, active int) (Meta, error) {
	b := meta0
	if active == 1 {
		b = meta1
	}
	m := DecodeMeta(b)
	if err := ValidateMeta(m); err != nil {
		return Meta{}, fmt.Errorf("pager: %w: %w", ErrCorrupted, err)
	}
	return m, nil
}

// errBothMetasInvalid is the shared selection-failure error: no valid
// slot, or an equal-non-zero TxnID pair (commit-protocol violation).
func errBothMetasInvalid() error {
	return fmt.Errorf("pager: both meta pages invalid or commit-protocol violation: %w", ErrCorrupted)
}

// attachState (re)builds the pager's in-memory state from the on-disk image
// for active meta m: refreshes fileSize, rebuilds the allocation bitmap from
// the on-disk bitmap region, rebuilds the RPL in-memory chain, and seeds
// commit state. Shared by Open (first build) and Resync (rebuild after a peer
// commit). The mmap is NOT touched — MaxSize and PageSize are immutable for
// the file's life, so the reservation established at NewWriter always covers
// the current file (a peer can grow the file only up to MaxSize, within the
// existing sparse mapping).
//
// attachState is ATOMIC: it builds the new bitmap and RPL chain into locals
// and installs them (fileSize, bitmap, RPL, commit-state) only after both
// succeed. Any error therefore leaves the pager fully unmodified, so the
// caller can release the grant and return the error WITHOUT poisoning the
// handle — Resync's stale-but-valid existing state is untouched, and Open
// simply closes the freshly-built pager it was loading.
//
// The freshly-stat'd file size bounds both the bitmap-extent check and the RPL
// walk (a peer may have grown or shrunk the file since this pager's mmap was
// established). The bitmap region lives in low pages [2, 2+BitmapPages) which
// maybeShrink never truncates, so the bitmap copy below is always within the
// backed extent.
func (p *Pager) attachState(file *os.File, m Meta) error {
	st, err := file.Stat()
	if err != nil {
		return fmt.Errorf("pager: stat: %w", err)
	}
	newFileSize := st.Size()
	pageSize := p.cfg.PageSize

	// Build the in-memory bitmap by reading the on-disk bitmap region from
	// the mmap (MAP_SHARED — sees committed data through the unified page
	// cache).
	//
	// ValidateMeta does not bound BitmapPages, so a checksum-passing meta
	// with a forged BitmapPages bypasses every existing guard. Two reachable
	// in-spec corruption shapes that must surface as ErrCorrupted, not crash
	// the process — checksums.md §Structural and Allocation Bounds and
	// integrity.md §Forged / structural corruption tolerance ("the read
	// path... never crashes on corrupt input — it returns an error
	// instead"):
	//
	//   (a) wild-high BitmapPages → `make([]byte, BitmapPages*PageSize)`
	//       hits runtime OOM via `runtime.throw` (unrecoverable —
	//       `recover()` does NOT catch this; the test binary dies), or at a
	//       smaller-but-still-too-big size the subsequent slice expression
	//       `p.mmap[2*pageSize : 2*pageSize+bitmapBytes]` panics with
	//       slice-bounds-out-of-range (recoverable, but the wild_high
	//       regression exercises the runtime.throw path — a stronger
	//       no-crash assertion).
	//   (b) BitmapPages too small to cover MaxSize (incl. zero) →
	//       `bitmap.New` panics with "totalPages N exceeds bitmap capacity M
	//       bits" (entered with empty/under-sized detail).
	//
	// File-extent bound (a): firstDataPage = 2 + BitmapPages must lie within
	// the file-resident extent. Use `min(fileSize/PageSize, MaxSize)` exactly
	// like rebuildRPLChain (checksums.md §Structural and Allocation Bounds) and checker.walkRPL (api-surface.md §Check, CopyTo, Compact) —
	// ValidateMeta deliberately does not enforce these (avoid rejecting
	// recoverable databases). Capacity bound (b): BitmapPages*PageSize*8 bits
	// must be >= MaxSize pages.
	backedPages := min(uint64(newFileSize)/uint64(pageSize), m.MaxSize)
	firstDataPage := uint64(m.BitmapPages) + 2
	if firstDataPage > backedPages {
		return fmt.Errorf("pager: meta firstDataPage %d (BitmapPages %d + 2 metas) exceeds backed extent %d pages: %w", firstDataPage, m.BitmapPages, backedPages, ErrCorrupted)
	}
	bitmapCapacityBits := uint64(m.BitmapPages) * uint64(pageSize) * 8
	if bitmapCapacityBits < m.MaxSize {
		return fmt.Errorf("pager: meta BitmapPages %d gives bitmap capacity %d bits, < MaxSize %d pages: %w", m.BitmapPages, bitmapCapacityBits, m.MaxSize, ErrCorrupted)
	}
	bitmapBytes := uint64(m.BitmapPages) * uint64(pageSize)
	detail := make([]byte, bitmapBytes)
	copy(detail, p.mmap[2*uint64(pageSize):2*uint64(pageSize)+bitmapBytes])
	bm := bitmap.New(detail, pageSize, m.BitmapPages, m.MaxSize)

	// Rebuild the RPL in-memory chain against the NEW bitmap + file size
	// (locals — not yet installed on the pager), walking head → tail via
	// OlderSegment, reversed for tail-first reclamation iteration. Building
	// against locals is what makes attachState atomic: a corrupt chain returns
	// here with the pager still fully unmodified.
	chain, err := rebuildRPLChain(p, m, bm, newFileSize)
	if err != nil {
		return fmt.Errorf("pager: rebuild RPL chain: %w", err)
	}

	// All fallible work is done — install every piece of state at once. None
	// of these assignments can fail, so the pager moves from fully-old to
	// fully-new with no observable partial state (the caller holds the write
	// grant). The reclamation bound seeded here is the meta's persisted
	// anchored epoch; the DB layer re-derives it per-tx as
	// min(oldestReader, AnchoredEpoch) (free-space.md §RPL Reclamation).
	p.fileSize = newFileSize
	p.AttachBitmap(bm)
	p.SetRPLChain(chain)
	p.advanceAnchoredEpoch(m.Durable.AnchoredTxnID)
	p.SetCommitState(m.HighWaterMark, m.MaxSize, p.anchoredEpoch)
	p.setSizeParams(m.GrowStep, m.MinSize)
	return nil
}

// Resync rebuilds the writer pager's in-memory state from the current on-disk
// image after a peer process may have committed (cross-process.md §Writer
// acquisition flow). The caller MUST hold the cross-process write grant, so no
// concurrent writer mutates the metas/bitmap/RPL and no tx is in flight (the
// bitmap is replaced wholesale).
//
// Selection is the ONE rule shared with Open and readers (ActiveMeta,
// highest valid TxnID); Resync uses its LIVE projection — a grant handoff is
// not recovery: the peer cleanly committed and released the flock, so its
// latest commit — even an unfsynced SyncLazy one — is complete and visible
// (same-host page cache), and rolling back to the durable epoch would
// silently lose it (durability.md §One selection, two projections).
//
// Anchoring: the persisted AnchoredDurableTxnID of the adopted meta advances
// the pager's in-process anchored epoch (monotone max — our own completed
// fsyncs stay valid). The reclamation bound derives from AnchoredEpoch
// (free-space.md §RPL Reclamation).
//
// knownTxnID is the caller's cached active-meta TxnID. When the on-disk latest
// meta still carries it, no peer commit has landed: Resync returns
// changed=false and rebuilds nothing. Only a genuine peer advance triggers
// the bitmap+RPL rebuild. The mmap is reused (MaxSize / PageSize immutable for
// the file's life).
//
// On a corrupt on-disk image (both metas invalid, forged BitmapPages, corrupt
// RPL chain) or a meta-read I/O error, Resync returns a wrapped error with the
// pager left **fully unmodified** (attachState is atomic and the read/select
// steps precede it) — except the monotone anchored-epoch advance, which is
// always safe — so the caller releases the grant and returns the error
// without poisoning — the handle stays usable (a retry re-reads; Close +
// re-Open invokes Open's own corruption recovery).
func (p *Pager) Resync(file *os.File, knownTxnID uint64) (m Meta, active int, changed bool, err error) {
	meta0, meta1, err := readMetaPair(file, p.cfg.PageSize)
	if err != nil {
		return Meta{}, 0, false, err
	}
	active, ok := ActiveMeta(meta0, meta1)
	if !ok {
		return Meta{}, 0, false, errBothMetasInvalid()
	}
	m, err = decodeActiveMeta(meta0, meta1, active)
	if err != nil {
		return Meta{}, 0, false, err
	}
	p.advanceAnchoredEpoch(m.Durable.AnchoredTxnID)
	if m.TxnID == knownTxnID {
		return m, active, false, nil
	}
	if err := p.attachState(file, m); err != nil {
		return Meta{}, 0, false, err
	}
	return m, active, true, nil
}

// RecoverToDurable runs the gated writable-Open recovery path
// (durability.md §Recovery steps 1–5) UNDER the caller's write grant:
// it re-reads both meta slots (the pre-grant Open snapshot can be
// stale by any number of peer commits that landed while AcquireWriter
// blocked — publishing from it would clobber an acked peer commit and
// retreat the durable epoch), selects the latest valid meta, and:
//
//   - Self-durable: attaches the live (== durable) projection, then
//     anchors the assertion by rewriting the meta to its own slot and
//     fsyncing (the meta may have been read from a surviving page
//     cache; §Anchoring), and returns it. recovered = false.
//   - Otherwise: attaches the DURABLE projection (walking the RPL
//     with the adopted epoch as the reclaim reference, so the epoch's
//     own head keeps its hard-error exemption), then publishes the
//     recovery commit at TxnID+1 to the non-selected slot and fsyncs.
//     recovered = true.
//
// The recovery meta's live fields are the adopted durable state, so
// its self-durable assertion (DurableTxnID = TxnID+1) is data-safe
// even before the fsync completes: the tree it names IS the durable
// epoch's. Its persisted AnchoredDurableTxnID is the ADOPTED epoch —
// the assertion just read from disk — per §Anchoring's
// no-forward-promise rule; the in-process anchored epoch advances to
// TxnID+1 only after the fsync returns. Idempotent under crash: a
// crash before the fsync leaves the old slots authoritative and
// recovery re-runs. The caller MUST hold the write grant and have
// passed the no-live-author gate.
func (p *Pager) RecoverToDurable(file *os.File) (m Meta, active int, recovered bool, err error) {
	meta0, meta1, err := readMetaPair(file, p.cfg.PageSize)
	if err != nil {
		return Meta{}, 0, false, err
	}
	selectedIdx, ok := ActiveMeta(meta0, meta1)
	if !ok {
		return Meta{}, 0, false, errBothMetasInvalid()
	}
	selected, err := decodeActiveMeta(meta0, meta1, selectedIdx)
	if err != nil {
		return Meta{}, 0, false, err
	}
	if selected.SelfDurable() {
		if err := p.attachState(file, selected); err != nil {
			return Meta{}, 0, false, err
		}
		// Anchor the assertion by re-writing the meta to its own slot
		// and fsyncing. The pwrite is load-bearing, not redundant: the
		// meta may live only in a surviving page cache, and a PRIOR
		// failed fsync (this process's or a crashed retry's) both
		// consumed the kernel's writeback error and marked the pages
		// clean — a bare fdatasync would then succeed trivially,
		// anchoring an assertion the disk never received, and
		// reclamation would free segments a power loss still needs
		// (durability.md §Anchoring). Re-dirtying the slot with the
		// byte-identical meta makes trivial success impossible; a torn
		// write of identical bytes is harmless and the other slot is
		// untouched.
		buf := make([]byte, p.cfg.PageSize)
		EncodeMeta(buf, &selected)
		if _, err := file.WriteAt(buf, int64(selectedIdx)*int64(p.cfg.PageSize)); err != nil {
			return Meta{}, 0, false, fmt.Errorf("pager: anchor rewrite meta %d: %w", selectedIdx, err)
		}
		if err := openFsync(file, "anchor"); err != nil {
			return Meta{}, 0, false, fmt.Errorf("pager: anchor fdatasync: %w", err)
		}
		p.advanceAnchoredEpoch(selected.Durable.TxnID)
		return selected, selectedIdx, false, nil
	}

	// Attach the durable projection FIRST, as a self-consistent meta at
	// the adopted epoch (live fields = durable fields, sub-record =
	// selected's): the RPL walk then runs with ReclaimEpoch = the
	// adopted epoch, so the epoch's OWN head keeps its hard-error
	// exemption (free-space.md §Head classification). Nothing has been
	// written yet — an attach failure leaves the disk untouched.
	d := selected.Durable
	proj := selected // UUID, format fields, immutables carry over
	proj.HighWaterMark = d.HighWaterMark
	proj.RPLHeadPage = d.RPLHeadPage
	proj.RPLHeadTxnID = d.RPLHeadTxnID
	proj.RPLTailPage = d.RPLTailPage
	proj.RPLEntryCount = d.RPLEntryCount
	proj.NumFreePages = d.NumFreePages
	proj.KeyspaceRoot = d.KeyspaceRoot
	proj.NumKeyspaces = d.NumKeyspaces
	proj.TxnID = d.TxnID
	if err := p.attachState(file, proj); err != nil {
		return Meta{}, 0, false, err
	}

	// Publish the recovery commit at selected.TxnID+1.
	rm := proj
	rm.TxnID = selected.TxnID + 1
	rm.Durable = rm.LiveSubRecord()
	rm.Durable.AnchoredTxnID = d.TxnID
	buf := make([]byte, p.cfg.PageSize)
	EncodeMeta(buf, &rm)
	newIdx := 1 - selectedIdx
	if _, err := file.WriteAt(buf, int64(newIdx)*int64(p.cfg.PageSize)); err != nil {
		return Meta{}, 0, false, fmt.Errorf("pager: recovery commit write meta %d: %w", newIdx, err)
	}
	if err := openFsync(file, "recovery-commit"); err != nil {
		return Meta{}, 0, false, fmt.Errorf("pager: recovery commit fdatasync: %w", err)
	}
	// The completed fsync anchors the recovery meta's own assertion;
	// re-seed the commit state to the published meta's view.
	p.advanceAnchoredEpoch(rm.Durable.TxnID)
	p.SetCommitState(rm.HighWaterMark, rm.MaxSize, p.anchoredEpoch)
	return rm, newIdx, true, nil
}

// ReadLatestMeta reads both on-disk meta pages and returns the latest
// COMMITTED one — the one selection, used in its LIVE projection: a read
// transaction wants the newest committed snapshot for visibility (it must
// observe a peer's completed commit, cross-process.md §Reader Table;
// durability.md §One selection, two projections). Lock-free: BeginRead holds no write grant, so a writer
// may be mid-commit on the inactive slot — a torn slot fails its checksum and
// ActiveMeta selects the valid one (the commit writes data pages before
// the meta, so the selected meta's pages are always readable). pageSize is the
// file's immutable page size (safely taken from any prior meta snapshot).
func ReadLatestMeta(file *os.File, pageSize uint32) (Meta, error) {
	meta0, meta1, err := readMetaPair(file, pageSize)
	if err != nil {
		return Meta{}, err
	}
	active, ok := ActiveMeta(meta0, meta1)
	if !ok {
		return Meta{}, errBothMetasInvalid()
	}
	return decodeActiveMeta(meta0, meta1, active)
}

// DiscoverPageSize returns the page size of the gmdb file by reading
// meta-0 and verifying its checksum + identity (Magic, Version). When
// meta-0 fails any of those, it probes meta-1 at each supported page
// size — the same fallback Open uses internally — exported so the gmdb
// root package can size its buffer pool before calling Open.
//
// Returns ErrCorrupted (wrapped) when no candidate produces a verifying
// meta. Propagates non-EOF read errors (EIO, permission, etc.) verbatim
// so the caller can distinguish a genuine I/O failure from a probe miss.
func DiscoverPageSize(file *os.File) (uint32, error) {
	meta0 := make([]byte, MetaPayloadSize)
	if _, err := file.ReadAt(meta0, 0); err != nil {
		return 0, fmt.Errorf("pager: read meta0: %w", err)
	}
	if isVersionMismatchMeta(meta0) {
		return 0, fmt.Errorf("pager: %w: meta0 version %d, want %d",
			ErrVersionMismatch, DecodeMeta(meta0).Version, page.FormatVersion)
	}
	if isGmdbMeta(meta0) {
		ps := DecodeMeta(meta0).PageSize
		if page.ValidPageSize(ps) {
			return ps, nil
		}
	}
	ps, _, err := probeMetaPageSize(file)
	if err != nil {
		return 0, fmt.Errorf("pager: meta1 probe read: %w", err)
	}
	if ps != 0 {
		return ps, nil
	}
	return 0, fmt.Errorf("pager: meta0 invalid and meta1 probe found no recoverable PageSize: %w", ErrCorrupted)
}

// probeMetaPageSize iterates the supported page sizes and looks for a
// recognizably-gmdb meta page at the corresponding offset whose
// PageSize field matches the offset. The first match wins; the
// returned bytes are the payload Open uses as meta-1 (already known to
// verify, so the caller can avoid a re-read).
//
// "Recognizably gmdb" requires the checksum to verify AND Magic +
// Version to match the package constants. Without the identity check
// any 144-byte slice that happens to be self-checksum-consistent would
// be accepted — a 2^-64 random match per offset, but a structured
// non-gmdb file (or a different-version gmdb file) could collide
// intentionally. The dual-meta atomicity invariant in file-layout.md
// requires recoverability when "one meta verifies at its proper
// offset"; "verifies" implies a gmdb meta, not an arbitrary blob.
//
// File-too-short reads return (0, nil, nil) — those offsets are
// legitimately absent. Other read errors (EIO, permission) bubble up
// so the caller can surface them rather than mislabel as "no
// recoverable PageSize."
func probeMetaPageSize(file *os.File) (uint32, []byte, error) {
	for ps := page.MinPageSize; ps <= page.MaxPageSize; ps *= 2 {
		buf := make([]byte, MetaPayloadSize)
		if _, err := file.ReadAt(buf, int64(ps)); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				continue
			}
			return 0, nil, err
		}
		if !isGmdbMeta(buf) {
			continue
		}
		if DecodeMeta(buf).PageSize == ps {
			return ps, buf, nil
		}
	}
	return 0, nil, nil
}

// isGmdbMeta reports whether buf is a recognizably-gmdb meta payload:
// the xxhash64 footer verifies AND Magic + Version match the package
// constants. Used by DiscoverPageSize and probeMetaPageSize as a
// single point of trust for "this 144-byte slice is one of our metas."
func isGmdbMeta(buf []byte) bool {
	if !VerifyMeta(buf) {
		return false
	}
	m := DecodeMeta(buf)
	return m.Magic == page.Magic && m.Version == page.FormatVersion
}

// isVersionMismatchMeta reports whether buf is an intact gmdb meta
// (checksum verifies, Magic matches) of a DIFFERENT format version — a
// valid gmdb file this binary cannot read, as opposed to a corrupt one.
// Mirror of isGmdbMeta with the Version condition flipped. The meta
// identity header (Magic@0, Version@4, checksum footer) is the
// version-stable contract that makes this classification possible
// across format evolutions (file-layout.md §Meta Page). Requiring the
// checksum to verify is what distinguishes a deliberately-written
// different-version file from bitrot that merely corrupted the Version
// field — the latter has no valid checksum and is ErrCorrupted.
func isVersionMismatchMeta(buf []byte) bool {
	if !VerifyMeta(buf) {
		return false
	}
	m := DecodeMeta(buf)
	return m.Magic == page.Magic && m.Version != page.FormatVersion
}

// rebuildRPLChain walks the on-disk RPL chain head → tail through the
// shared RPLChainWalk convention (rplwalk.go), then reverses the result
// so index 0 is tail (oldest). Truncation boundaries (stale tail on a
// non-latest meta) yield a shorter chain; hard walk errors surface as
// ErrCorrupted / ErrBadPageChecksum at Open.
//
// bm and fileSize are passed explicitly (rather than read from p.bitmap /
// p.fileSize) so attachState can rebuild against the NOT-yet-installed new
// state — keeping attachState atomic. bm is the reclaimed-segment oracle
// (free-space.md §Allocation Bitmap: set bit = free → stop at a reclaimed
// tail); fileSize bounds every segment page id to the file-resident extent.
func rebuildRPLChain(p *Pager, m Meta, bm *bitmap.Bitmap, fileSize int64) ([]RPLSegmentRef, error) {
	// Trustworthy ceiling for every segment page id: head, every followed
	// OlderSegment, and the tail. pageRaw panics past the mmap reservation
	// (MaxSize pages) and would SIGBUS in the [fileSize, reservation) gap,
	// so a corrupt meta whose RPLHeadPage / OlderSegment is out of range
	// must surface as ErrCorrupted at Open, not crash. The bound is the
	// file-resident extent capped by MaxSize — NOT meta.HighWaterMark
	// (ValidateMeta does not enforce HighWaterMark <= MaxSize, so a forged
	// meta can inflate it past the reservation) and NOT MaxSize alone (the
	// file may be shorter than the reservation). This is Pager.Page's
	// file-resident bound (checksums.md §Structural and Allocation Bounds).
	backedPages := min(uint64(fileSize)/uint64(p.cfg.PageSize), m.MaxSize)
	var headFirst []RPLSegmentRef
	walk := RPLChainWalk{
		ReadPage:     p.pageRaw,
		Cfg:          p.cfg,
		Head:         m.RPLHeadPage,
		HeadTxnID:    m.RPLHeadTxnID,
		Tail:         m.RPLTailPage,
		EntryCount:   m.RPLEntryCount,
		ReclaimEpoch: m.Durable.TxnID,
		// Below the meta/bitmap region no segment can live; reclaimRPL
		// would panic in Bitmap.Set on such an id, so it must not enter
		// the chain.
		LowBound:  bm.FirstDataPage(),
		HighBound: backedPages,
		IsFree:    func(id uint64) (bool, bool) { return bm.IsSet(id), true },
	}
	_, werr := walk.Walk(func(id uint64, seg RPLSegment) bool {
		headFirst = append(headFirst, RPLSegmentRef{
			PageID: id,
			TxnID:  seg.TxnID,
			Count:  uint32(len(seg.PageIDs)),
		})
		return true
	})
	if werr != nil {
		sentinel := ErrCorrupted
		if werr.Kind == RPLWalkErrHeadChecksum {
			sentinel = ErrBadPageChecksum
		}
		return nil, fmt.Errorf("pager: %s: %w", werr.Error(), sentinel)
	}
	// Reverse: head-first → tail-first.
	slices.Reverse(headFirst)
	return headFirst, nil
}

package pager

import (
	"errors"
	"fmt"
	"io"
	"os"

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
		flags |= page.MetaFlagPageChecksum
	}
	// At init, the "no transactions yet" state is also the latest
	// checkpoint. Set the Checkpoint flag so Open's durability-aware
	// caller sees this meta as the recovery target.
	flags |= page.MetaFlagCheckpoint

	m := page.Meta{
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
		RPLTailPage:     0,
		RPLEntryCount:   0,
		NumFreePages:    0,
		KeyspaceRoot:    0,
		NumKeyspaces:    0,
		TxnID:           0,
	}

	pageSizeI := int64(ip.PageSize)
	if err := file.Truncate(int64(filePages) * pageSizeI); err != nil {
		return fmt.Errorf("pager: truncate: %w", err)
	}

	// Write meta page 0 and meta page 1. Each occupies a full page; the
	// meta payload is the first MetaPayloadSize bytes and the rest of
	// the page is zero-filled.
	metaBuf := make([]byte, ip.PageSize)
	page.EncodeMeta(metaBuf, &m)
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

// OpenedDB bundles the products of Open: the writer pager (ready for
// write transactions), the active meta (snapshot), and the active-meta
// index. The caller advances PrevActive on commit and re-snapshots Meta
// from the post-commit return.
//
// NoCheckpoint surfaces the durability.md §Recovery step-3 case: the
// active meta was selected despite NOT having MetaFlagCheckpoint set.
// The root package's caller logs a warning via slog when this is true
// — recovery accepted a non-checkpoint meta because no
// checkpoint-flagged meta exists; data integrity depends on whether
// the OS flushed pages in the right order (SyncLazy-only DB never
// Checkpoint()'d).
type OpenedDB struct {
	Pager         *Pager
	Meta          page.Meta
	ActiveMetaIdx int
	NoCheckpoint  bool
}

// Open reads the file's two meta pages, selects the active one,
// validates its fields, mmaps the data file with a reservation of
// `Meta.MaxSize * PageSize`, builds the in-memory bitmap by reading the
// on-disk bitmap region, rebuilds the RPL in-memory segment list by
// walking the on-disk chain, and returns a writer pager ready for the
// first write transaction.
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
	m, active, noCheckpoint, pageSize, err := readAndSelectMeta(file)
	if err != nil {
		return nil, err
	}

	cfg := page.Config{PageSize: pageSize, PageChecksum: m.HasFlag(page.MetaFlagPageChecksum)}

	// 3) Reservation = MaxSize * PageSize, mmap, mprotect.
	reservation := int64(m.MaxSize) * int64(pageSize)
	p, err := NewWriter(file, cfg, reservation, op.Pool, op.MaxTxBufferBytes)
	if err != nil {
		return nil, err
	}

	// 4–6) Build the in-memory bitmap + RPL chain + commit state from the
	// on-disk image. Shared with Resync via attachState (which documents the
	// forged-BitmapPages corruption bounds it enforces).
	if err := p.attachState(file, m); err != nil {
		_ = p.Close()
		return nil, err
	}

	return &OpenedDB{
		Pager:         p,
		Meta:          m,
		ActiveMetaIdx: active,
		NoCheckpoint:  noCheckpoint,
	}, nil
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
	m, active, noCheckpoint, pageSize, err := readAndSelectMeta(file)
	if err != nil {
		return nil, err
	}
	cfg := page.Config{PageSize: pageSize, PageChecksum: m.HasFlag(page.MetaFlagPageChecksum)}
	reservation := int64(m.MaxSize) * int64(pageSize)
	p, err := NewReader(file, cfg, reservation)
	if err != nil {
		return nil, err
	}
	return &OpenedDB{
		Pager:         p,
		Meta:          m,
		ActiveMetaIdx: active,
		NoCheckpoint:  noCheckpoint,
	}, nil
}

// readAndSelectMeta reads the two meta-page images, discovers the
// PageSize (trusting meta-0's only when its checksum verifies,
// otherwise probing meta-1 candidate offsets — a passing checksum is
// the only thing that authorizes trust in any meta field, since
// ValidPageSize alone can't catch a checksum-breaking flip that still
// yields a syntactically valid value), and selects + validates the
// active meta (checkpoint-preferring, durability.md §Recovery step 3;
// noCheckpoint = the step-3 fallback where no checkpoint-flagged meta
// exists — the caller logs a warning). Shared by Open (then NewWriter +
// attachState) and OpenReadOnly (then NewReader).
func readAndSelectMeta(file *os.File) (m page.Meta, active int, noCheckpoint bool, pageSize uint32, err error) {
	meta0Bytes := make([]byte, page.MetaPayloadSize)
	if _, err := file.ReadAt(meta0Bytes, 0); err != nil {
		return page.Meta{}, 0, false, 0, fmt.Errorf("pager: read meta0: %w", err)
	}
	// An intact gmdb meta-0 of a different format version is reported
	// distinctly from corruption (file-layout.md §Meta Page): the file
	// is fine, this binary just can't read its format. Checked before
	// the recovery machinery so a different-version file never
	// masquerades as a torn/corrupt current-version file.
	if isVersionMismatchMeta(meta0Bytes) {
		return page.Meta{}, 0, false, 0, fmt.Errorf("pager: %w: meta0 version %d, want %d",
			ErrVersionMismatch, page.DecodeMeta(meta0Bytes).Version, page.FormatVersion)
	}
	var meta1Bytes []byte
	if isGmdbMeta(meta0Bytes) {
		pageSize = page.DecodeMeta(meta0Bytes).PageSize
		if !page.ValidPageSize(pageSize) {
			// Checksum agrees with a value that the format rejects:
			// the file was written by a different format version or
			// the checksum collided. Either way, ErrCorrupted.
			return page.Meta{}, 0, false, 0, fmt.Errorf("pager: meta0 verified but PageSize %d invalid: %w", pageSize, ErrCorrupted)
		}
	} else {
		var perr error
		pageSize, meta1Bytes, perr = probeMetaPageSize(file)
		if perr != nil {
			return page.Meta{}, 0, false, 0, fmt.Errorf("pager: meta1 probe read: %w", perr)
		}
		if pageSize == 0 {
			return page.Meta{}, 0, false, 0, fmt.Errorf("pager: meta0 verify failed and meta1 probe found no recoverable meta: %w", ErrCorrupted)
		}
	}
	if meta1Bytes == nil {
		meta1Bytes = make([]byte, page.MetaPayloadSize)
		if _, err := file.ReadAt(meta1Bytes, int64(pageSize)); err != nil {
			return page.Meta{}, 0, false, 0, fmt.Errorf("pager: read meta1: %w", err)
		}
	}
	m, active, noCheckpoint, err = selectActiveMeta(meta0Bytes, meta1Bytes)
	if err != nil {
		return page.Meta{}, 0, false, 0, err
	}
	return m, active, noCheckpoint, pageSize, nil
}

// selectActiveMeta decodes the two meta-page byte images, selects the active
// meta (checkpoint-preferring per durability.md §Recovery), and validates it.
// noCheckpoint reports the §Recovery step-3 fallback: the selected meta lacks
// MetaFlagCheckpoint because no checkpoint-flagged meta exists. Shared by Open
// (first load) and Resync (re-load after a peer commit).
func selectActiveMeta(meta0Bytes, meta1Bytes []byte) (m page.Meta, active int, noCheckpoint bool, err error) {
	active, noCheckpoint, ok := page.ActiveMetaCheckpointPreferring(meta0Bytes, meta1Bytes)
	if !ok {
		return page.Meta{}, 0, false, fmt.Errorf("pager: both meta pages invalid or commit-protocol violation: %w", ErrCorrupted)
	}
	if active == 0 {
		m = page.DecodeMeta(meta0Bytes)
	} else {
		m = page.DecodeMeta(meta1Bytes)
	}
	if err := page.ValidateMeta(m); err != nil {
		return page.Meta{}, 0, false, fmt.Errorf("pager: %w: %w", ErrCorrupted, err)
	}
	return m, active, noCheckpoint, nil
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
func (p *Pager) attachState(file *os.File, m page.Meta) error {
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
	bm := newBitmapForOpen(detail, pageSize, m.BitmapPages, m.MaxSize)

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
	// grant). The reclamation bound seeded here is the active meta's TxnID;
	// the DB layer overrides it per-tx with min(oldestReader, lastCheckpoint)
	// (free-space.md §RPL Reclamation).
	p.fileSize = newFileSize
	p.AttachBitmap(bm)
	p.SetRPLChain(chain)
	p.SetCommitState(m.HighWaterMark, m.MaxSize, m.TxnID)
	p.SetSizeParams(m.GrowStep, m.MinSize)
	return nil
}

// Resync rebuilds the writer pager's in-memory state from the current on-disk
// image after a peer process may have committed (cross-process.md §Writer
// acquisition flow). The caller MUST hold the cross-process write grant, so no
// concurrent writer mutates the metas/bitmap/RPL and no tx is in flight (the
// bitmap is replaced wholesale).
//
// Base-meta selection is LATEST-COMMITTED (page.ActiveMeta — highest valid
// TxnID), NOT the checkpoint-preferring selection Open uses. Open is crash
// recovery, where an uncheckpointed SyncLazy commit past the last checkpoint
// may be torn and must be rolled back. A live grant handoff is not recovery:
// the peer cleanly committed and released the flock, so its latest commit —
// even an uncheckpointed SyncLazy one — is complete and visible (same-host
// page cache), and rolling back to the last checkpoint would silently lose it
// (a lost update, and inconsistent with the latest-committed snapshot
// ReadLatestMeta hands to readers). This also matters single-process: with
// checkpoint-preferring, a writer's own SyncLazy commit would be rolled back
// on its next Begin.
//
// lastCheckpointTxnID is the highest checkpoint-flagged TxnID among the two
// on-disk slots (0 if none) — exactly the meta a crash would recover to
// (checkpoint-preferring), so it bounds RPL reclamation to protect that
// recoverable tree (free-space.md §RPL Reclamation). The caller adopts it only
// on changed=true; on changed=false it keeps its own in-memory tracking, which
// can be tighter (it remembers a checkpoint that SyncLazy commits have since
// overwritten in the slots).
//
// knownTxnID is the caller's cached active-meta TxnID. When the on-disk latest
// meta still carries it, no peer commit has landed: Resync returns
// changed=false and rebuilds nothing, so the caller keeps its cached meta AND
// its in-memory last-checkpoint tracking. Only a genuine peer advance triggers
// the bitmap+RPL rebuild. The mmap is reused (MaxSize / PageSize immutable for
// the file's life).
//
// On a corrupt on-disk image (both metas invalid, forged BitmapPages, corrupt
// RPL chain) or a meta-read I/O error, Resync returns a wrapped error with the
// pager left **fully unmodified** (attachState is atomic and the read/select
// steps precede it), so the caller releases the grant and returns the error
// without poisoning — the handle stays usable (a retry re-reads; Close +
// re-Open invokes Open's own corruption recovery).
func (p *Pager) Resync(file *os.File, knownTxnID uint64) (m page.Meta, active int, lastCheckpointTxnID uint64, changed bool, err error) {
	pageSize := p.cfg.PageSize
	meta0 := make([]byte, page.MetaPayloadSize)
	if _, err := file.ReadAt(meta0, 0); err != nil {
		return page.Meta{}, 0, 0, false, fmt.Errorf("pager: resync read meta0: %w", err)
	}
	meta1 := make([]byte, page.MetaPayloadSize)
	if _, err := file.ReadAt(meta1, int64(pageSize)); err != nil {
		return page.Meta{}, 0, 0, false, fmt.Errorf("pager: resync read meta1: %w", err)
	}
	active, ok := page.ActiveMeta(meta0, meta1)
	if !ok {
		return page.Meta{}, 0, 0, false, fmt.Errorf("pager: both meta pages invalid or commit-protocol violation: %w", ErrCorrupted)
	}
	if active == 0 {
		m = page.DecodeMeta(meta0)
	} else {
		m = page.DecodeMeta(meta1)
	}
	if err := page.ValidateMeta(m); err != nil {
		return page.Meta{}, 0, 0, false, fmt.Errorf("pager: %w: %w", ErrCorrupted, err)
	}
	lastCheckpointTxnID = highestCheckpointTxnID(meta0, meta1)
	if m.TxnID == knownTxnID {
		return m, active, lastCheckpointTxnID, false, nil
	}
	if err := p.attachState(file, m); err != nil {
		return page.Meta{}, 0, 0, false, err
	}
	return m, active, lastCheckpointTxnID, true, nil
}

// highestCheckpointTxnID returns the greatest TxnID among the two meta-page
// images that both verify (checksum) AND carry MetaFlagCheckpoint, or 0 if
// neither qualifies. This is the TxnID a crash would recover to under the
// checkpoint-preferring rule, hence the RPL reclamation lower bound for a
// writer that has just adopted a peer's latest (possibly unflagged) commit.
func highestCheckpointTxnID(meta0, meta1 []byte) uint64 {
	var best uint64
	for _, b := range [][]byte{meta0, meta1} {
		if !page.VerifyMeta(b) {
			continue
		}
		mm := page.DecodeMeta(b)
		if mm.HasFlag(page.MetaFlagCheckpoint) && mm.TxnID > best {
			best = mm.TxnID
		}
	}
	return best
}

// ReadLatestMeta reads both on-disk meta pages and returns the latest
// COMMITTED one — the highest valid TxnID, NOT checkpoint-preferring. A read
// transaction wants the newest committed snapshot for visibility (it must
// observe a peer's completed commit, cross-process.md §Reader Table), whereas
// recovery/Open want the newest durable checkpoint; the two selections differ
// only when the newest commit is an unflagged SyncLazy commit, where a reader
// correctly prefers it. Lock-free: BeginRead holds no write grant, so a writer
// may be mid-commit on the inactive slot — a torn slot fails its checksum and
// page.ActiveMeta selects the valid one (the commit writes data pages before
// the meta, so the selected meta's pages are always readable). pageSize is the
// file's immutable page size (safely taken from any prior meta snapshot).
func ReadLatestMeta(file *os.File, pageSize uint32) (page.Meta, error) {
	meta0 := make([]byte, page.MetaPayloadSize)
	if _, err := file.ReadAt(meta0, 0); err != nil {
		return page.Meta{}, fmt.Errorf("pager: read meta0: %w", err)
	}
	meta1 := make([]byte, page.MetaPayloadSize)
	if _, err := file.ReadAt(meta1, int64(pageSize)); err != nil {
		return page.Meta{}, fmt.Errorf("pager: read meta1: %w", err)
	}
	active, ok := page.ActiveMeta(meta0, meta1)
	if !ok {
		return page.Meta{}, fmt.Errorf("pager: both meta pages invalid or commit-protocol violation: %w", ErrCorrupted)
	}
	var m page.Meta
	if active == 0 {
		m = page.DecodeMeta(meta0)
	} else {
		m = page.DecodeMeta(meta1)
	}
	if err := page.ValidateMeta(m); err != nil {
		return page.Meta{}, fmt.Errorf("pager: %w: %w", ErrCorrupted, err)
	}
	return m, nil
}

// newBitmapForOpen is a thin wrapper that returns the bitmap package's
// constructed Bitmap. Co-located with the cross-package import in
// bitmapwrap.go.
func newBitmapForOpen(detail []byte, pageSize uint32, bitmapPages uint32, totalPages uint64) *bitmapForOpen {
	return bitmapWrap(detail, pageSize, bitmapPages, totalPages)
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
	meta0 := make([]byte, page.MetaPayloadSize)
	if _, err := file.ReadAt(meta0, 0); err != nil {
		return 0, fmt.Errorf("pager: read meta0: %w", err)
	}
	if isVersionMismatchMeta(meta0) {
		return 0, fmt.Errorf("pager: %w: meta0 version %d, want %d",
			ErrVersionMismatch, page.DecodeMeta(meta0).Version, page.FormatVersion)
	}
	if isGmdbMeta(meta0) {
		ps := page.DecodeMeta(meta0).PageSize
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
		buf := make([]byte, page.MetaPayloadSize)
		if _, err := file.ReadAt(buf, int64(ps)); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				continue
			}
			return 0, nil, err
		}
		if !isGmdbMeta(buf) {
			continue
		}
		if page.DecodeMeta(buf).PageSize == ps {
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
	if !page.VerifyMeta(buf) {
		return false
	}
	m := page.DecodeMeta(buf)
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
	if !page.VerifyMeta(buf) {
		return false
	}
	m := page.DecodeMeta(buf)
	return m.Magic == page.Magic && m.Version != page.FormatVersion
}

// rebuildRPLChain walks the on-disk RPL chain head → tail via
// OlderSegment links, then reverses the result so index 0 is tail
// (oldest). Defense in depth: refuses to walk a self-referential
// segment (OlderSegment == own page ID) and bounds the walk by the
// meta's RPLEntryCount divided by the minimum entries-per-segment
// (with slack) to catch chain cycles or wild OlderSegment pointers
// before they cause an infinite loop.
//
// bm and fileSize are passed explicitly (rather than read from p.bitmap /
// p.fileSize) so attachState can rebuild against the NOT-yet-installed new
// state — keeping attachState atomic. bm is the reclaimed-segment oracle
// (free-space.md §Allocation Bitmap: set bit = free → stop at a reclaimed
// tail); fileSize bounds every segment page id to the file-resident extent.
func rebuildRPLChain(p *Pager, m page.Meta, bm *bitmapForOpen, fileSize int64) ([]RPLSegmentRef, error) {
	if m.RPLHeadPage == 0 {
		return nil, nil
	}
	// Upper bound on segment count: every segment holds ≥1 entry, so
	// a valid chain has at most RPLEntryCount segments. The +1 slack
	// covers the trivial empty-chain case (RPLEntryCount==0 with a
	// stale RPLHeadPage from a partial pwrite would be one excess
	// segment); cycles are caught by the visited-set, so the count
	// bound is a belt-and-suspenders second line of defense.
	// The chain is bounded by the authoritative tail pointer, NOT by
	// OlderSegment == 0: reclaimRPL drains whole segments from the tail
	// (oldest) without rewriting the new tail's on-disk OlderSegment, so that
	// pointer dangles at a reclaimed (and possibly reused) page. Walking it
	// would read a non-segment page as a segment. RPLTailPage is recomputed on
	// every commit (buildNewMeta) from the surviving chain, so it is the
	// correct terminator. head and tail are both zero or both non-zero
	// (buildNewMeta); a head with no tail is corrupt meta.
	tail := m.RPLTailPage
	if tail == 0 {
		return nil, fmt.Errorf("pager: RPL head %d set but tail is 0: %w", m.RPLHeadPage, ErrCorrupted)
	}
	maxSegs := m.RPLEntryCount + 1
	visited := make(map[uint64]struct{}, maxSegs)
	var headFirst []RPLSegmentRef
	// Trustworthy ceiling for every segment page id, computed BEFORE the
	// walk so it bounds head, every followed OlderSegment, and the tail.
	// pageRaw panics past the mmap reservation (MaxSize pages) and would
	// SIGBUS in the [fileSize, reservation) gap, so a corrupt meta whose
	// RPLHeadPage / OlderSegment is out of range must surface as
	// ErrCorrupted at Open, not crash. The bound is the file-resident
	// extent capped by MaxSize — NOT meta.HighWaterMark (ValidateMeta does
	// not enforce HighWaterMark <= MaxSize, so a forged meta can inflate it
	// past the reservation) and NOT MaxSize alone (the file may be shorter
	// than the reservation). This is Pager.Page's file-resident bound (checksums.md §Structural and Allocation Bounds), identical
	// to checker.walkRPL's min(fileSize/PageSize, MaxSize) (the root gmdb
	// package's check.go); the two sibling RPL walkers must agree a wild
	// pointer is structured corruption, not a crash.
	backedPages := min(uint64(fileSize)/uint64(p.cfg.PageSize), m.MaxSize)
	id := m.RPLHeadPage
	for {
		if id >= backedPages {
			return nil, fmt.Errorf("pager: RPL segment page %d beyond file-resident extent %d pages: %w", id, backedPages, ErrCorrupted)
		}
		if _, seen := visited[id]; seen {
			return nil, fmt.Errorf("pager: RPL chain cycle at page %d: %w", id, ErrCorrupted)
		}
		if uint64(len(headFirst)) > maxSegs {
			return nil, fmt.Errorf("pager: RPL chain exceeds bound %d (likely cycle): %w", maxSegs, ErrCorrupted)
		}
		// Stop at a reclaimed segment. Recovery may select a NON-latest meta
		// (an older checkpoint, durability.md §Recovery); reclamation drains +
		// frees segment pages from the live tail and advances the live meta's
		// RPLTailPage WITHOUT rewriting older metas, so an older meta's
		// RPLHeadPage→OlderSegment walk can reach a segment whose page was
		// reclaimed (and possibly reused). A reclaimed segment page is free in
		// the bitmap (free-space.md §Allocation Bitmap: set bit = free), and
		// its listed data pages are already free, so truncating the in-memory
		// chain here is consistent with the bitmap. This never applies to the
		// head: the recovery target's own newest segment is never reclaimed
		// (the reclamation bound never reaches the last checkpoint's own
		// TxnID). See free-space.md §RPL (recovery to a non-latest meta).
		if id != m.RPLHeadPage && bm.IsSet(id) {
			break
		}
		visited[id] = struct{}{}
		buf := p.pageRaw(id)
		seg, ok := page.DecodeRPLSegment(buf, p.cfg)
		if !ok {
			if id == m.RPLHeadPage {
				return nil, fmt.Errorf("pager: RPL head segment at page %d malformed: %w", id, ErrCorrupted)
			}
			// A reclaimed-then-reused segment page (now holding non-segment
			// data): same stale-tail boundary as the reclaimed-bit check above.
			break
		}
		headFirst = append(headFirst, RPLSegmentRef{
			PageID: id,
			TxnID:  seg.TxnID,
			Count:  uint32(len(seg.PageIDs)),
		})
		if id == tail {
			break // authoritative tail — do not follow the (possibly dangling) OlderSegment
		}
		next := seg.OlderSegment
		if next == 0 {
			return nil, fmt.Errorf("pager: RPL chain from head %d ended before tail %d: %w", m.RPLHeadPage, tail, ErrCorrupted)
		}
		if next == id {
			return nil, fmt.Errorf("pager: RPL segment at page %d is self-referential: %w", id, ErrCorrupted)
		}
		id = next
	}
	// Reverse: head-first → tail-first.
	for i, j := 0, len(headFirst)-1; i < j; i, j = i+1, j-1 {
		headFirst[i], headFirst[j] = headFirst[j], headFirst[i]
	}
	return headFirst, nil
}

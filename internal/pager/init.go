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
	// caller (chunk 3) sees this meta as the recovery target.
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
// chunk-1 mechanism that honours it.
func Open(file *os.File, op OpenParams) (*OpenedDB, error) {
	if op.Pool == nil {
		return nil, fmt.Errorf("pager: Pool must not be nil")
	}
	if op.MaxTxBufferBytes <= 0 {
		return nil, fmt.Errorf("pager: MaxTxBufferBytes must be > 0")
	}
	// 1) Discover PageSize. Prefer meta-0's PageSize when meta-0
	//    verifies; otherwise probe meta-1 candidate offsets. A passing
	//    checksum is the only thing that authorizes trust in any meta
	//    field — `ValidPageSize` alone is not enough because a flip
	//    that breaks the checksum can still produce a syntactically
	//    valid (but wrong) PageSize value.
	meta0Bytes := make([]byte, page.MetaPayloadSize)
	if _, err := file.ReadAt(meta0Bytes, 0); err != nil {
		return nil, fmt.Errorf("pager: read meta0: %w", err)
	}
	var pageSize uint32
	var meta1Bytes []byte
	if isGmdbMeta(meta0Bytes) {
		pageSize = page.DecodeMeta(meta0Bytes).PageSize
		if !page.ValidPageSize(pageSize) {
			// Checksum agrees with a value that the format rejects:
			// the file was written by a different format version or
			// the checksum collided. Either way, ErrCorrupted.
			return nil, fmt.Errorf("pager: meta0 verified but PageSize %d invalid: %w", pageSize, ErrCorrupted)
		}
	} else {
		var perr error
		pageSize, meta1Bytes, perr = probeMetaPageSize(file)
		if perr != nil {
			return nil, fmt.Errorf("pager: meta1 probe read: %w", perr)
		}
		if pageSize == 0 {
			return nil, fmt.Errorf("pager: meta0 verify failed and meta1 probe found no recoverable meta: %w", ErrCorrupted)
		}
	}
	if meta1Bytes == nil {
		meta1Bytes = make([]byte, page.MetaPayloadSize)
		if _, err := file.ReadAt(meta1Bytes, int64(pageSize)); err != nil {
			return nil, fmt.Errorf("pager: read meta1: %w", err)
		}
	}

	// 2) Active-meta selection + validation. Chunk-3.5: prefer
	// the highest-TxnID valid meta whose MetaFlagCheckpoint is set;
	// fall back to highest-TxnID valid meta if no checkpoint exists
	// (durability.md §Recovery step 3 — caller logs warning via
	// noCheckpoint).
	active, noCheckpoint, ok := page.ActiveMetaCheckpointPreferring(meta0Bytes, meta1Bytes)
	if !ok {
		return nil, fmt.Errorf("pager: both meta pages invalid or commit-protocol violation: %w", ErrCorrupted)
	}
	var m page.Meta
	if active == 0 {
		m = page.DecodeMeta(meta0Bytes)
	} else {
		m = page.DecodeMeta(meta1Bytes)
	}
	if err := page.ValidateMeta(m); err != nil {
		return nil, fmt.Errorf("pager: %w: %w", ErrCorrupted, err)
	}

	cfg := page.Config{PageSize: pageSize, PageChecksum: m.HasFlag(page.MetaFlagPageChecksum)}

	// 3) Reservation = MaxSize * PageSize, mmap, mprotect.
	reservation := int64(m.MaxSize) * int64(pageSize)
	p, err := NewWriter(file, cfg, reservation, op.Pool, op.MaxTxBufferBytes)
	if err != nil {
		return nil, err
	}

	// 4) Build the in-memory bitmap by reading the on-disk bitmap region
	// from the mmap (which sees the just-written data through the
	// unified page cache).
	bitmapBytes := uint64(m.BitmapPages) * uint64(pageSize)
	detail := make([]byte, bitmapBytes)
	copy(detail, p.mmap[2*uint64(pageSize):2*uint64(pageSize)+bitmapBytes])
	bm := newBitmapForOpen(detail, pageSize, m.BitmapPages, m.MaxSize)
	p.AttachBitmap(bm)

	// 5) Rebuild RPL in-memory chain: walk head → tail via OlderSegment,
	// reverse for tail-first iteration during reclamation.
	chain, err := rebuildRPLChain(p, m)
	if err != nil {
		_ = p.Close()
		return nil, fmt.Errorf("pager: rebuild RPL chain: %w", err)
	}
	p.SetRPLChain(chain)

	// 6) Seed commit state: HighWaterMark, MaxSize, reclamationBound.
	// For chunk 1 (no readers, every commit is a checkpoint) the
	// reclamation bound is the active meta's TxnID — segments freed
	// strictly before this TxnID are reclaimable.
	p.SetCommitState(m.HighWaterMark, m.MaxSize, m.TxnID)

	return &OpenedDB{
		Pager:         p,
		Meta:          m,
		ActiveMetaIdx: active,
		NoCheckpoint:  noCheckpoint,
	}, nil
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

// rebuildRPLChain walks the on-disk RPL chain head → tail via
// OlderSegment links, then reverses the result so index 0 is tail
// (oldest). Defense in depth: refuses to walk a self-referential
// segment (OlderSegment == own page ID) and bounds the walk by the
// meta's RPLEntryCount divided by the minimum entries-per-segment
// (with slack) to catch chain cycles or wild OlderSegment pointers
// before they cause an infinite loop.
func rebuildRPLChain(p *Pager, m page.Meta) ([]RPLSegmentRef, error) {
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
	// than the reservation). This is Pager.Page's Inv-RV3 bound, identical
	// to checker.walkRPL's min(fileSize/PageSize, MaxSize) (the root gmdb
	// package's check.go); the two sibling RPL walkers must agree a wild
	// pointer is structured corruption, not a crash.
	backedPages := min(uint64(p.fileSize)/uint64(p.cfg.PageSize), m.MaxSize)
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
		visited[id] = struct{}{}
		buf := p.pageRaw(id)
		seg, ok := page.DecodeRPLSegment(buf, p.cfg)
		if !ok {
			return nil, fmt.Errorf("pager: RPL segment at page %d malformed: %w", id, ErrCorrupted)
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

package pager

import (
	"fmt"
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
type OpenedDB struct {
	Pager         *Pager
	Meta          page.Meta
	ActiveMetaIdx int
}

// Open reads the file's two meta pages, selects the active one,
// validates its fields, mmaps the data file with a reservation of
// `Meta.MaxSize * PageSize`, builds the in-memory bitmap by reading the
// on-disk bitmap region, rebuilds the RPL in-memory segment list by
// walking the on-disk chain, and returns a writer pager ready for the
// first write transaction.
//
// The returned Pager's pool is op.Pool; it must outlive the pager.
func Open(file *os.File, op OpenParams) (*OpenedDB, error) {
	if op.Pool == nil {
		return nil, fmt.Errorf("pager: Pool must not be nil")
	}
	if op.MaxTxBufferBytes <= 0 {
		return nil, fmt.Errorf("pager: MaxTxBufferBytes must be > 0")
	}
	// 1) Read both meta-page payloads. We don't know PageSize yet, so we
	//    read MetaPayloadSize bytes from byte 0 and from PageSize away —
	//    but PageSize is unknown. Strategy: read enough at offset 0 to
	//    decode meta0 (MetaPayloadSize=144 bytes), validate it, then
	//    use meta0.PageSize to locate meta1. (If meta0 is corrupt, fall
	//    back to scanning a range of page-size guesses to find meta1.
	//    For chunk 1 we assume meta0 is recoverable; chunk 11 Check()
	//    addresses the fully-corrupt-meta0 case.)
	meta0Bytes := make([]byte, page.MetaPayloadSize)
	if _, err := file.ReadAt(meta0Bytes, 0); err != nil {
		return nil, fmt.Errorf("pager: read meta0: %w", err)
	}
	m0 := page.DecodeMeta(meta0Bytes)
	if !page.ValidPageSize(m0.PageSize) {
		return nil, fmt.Errorf("pager: meta0 PageSize invalid (%d); chunk-1 Open requires a recoverable meta0", m0.PageSize)
	}
	pageSize := m0.PageSize
	meta1Bytes := make([]byte, page.MetaPayloadSize)
	if _, err := file.ReadAt(meta1Bytes, int64(pageSize)); err != nil {
		return nil, fmt.Errorf("pager: read meta1: %w", err)
	}

	// 2) Active-meta selection + validation.
	active, ok := page.ActiveMeta(meta0Bytes, meta1Bytes)
	if !ok {
		return nil, fmt.Errorf("pager: both meta pages invalid or commit-protocol violation")
	}
	var m page.Meta
	if active == 0 {
		m = m0
	} else {
		m = page.DecodeMeta(meta1Bytes)
	}
	if err := page.ValidateMeta(m); err != nil {
		return nil, fmt.Errorf("pager: %w", err)
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
	}, nil
}

// newBitmapForOpen is a thin wrapper that returns the bitmap package's
// constructed Bitmap. Co-located with the cross-package import in
// bitmapwrap.go.
func newBitmapForOpen(detail []byte, pageSize uint32, bitmapPages uint32, totalPages uint64) *bitmapForOpen {
	return bitmapWrap(detail, pageSize, bitmapPages, totalPages)
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
	maxSegs := m.RPLEntryCount + 1
	visited := make(map[uint64]struct{}, maxSegs)
	var headFirst []RPLSegmentRef
	id := m.RPLHeadPage
	for id != 0 {
		if _, seen := visited[id]; seen {
			return nil, fmt.Errorf("pager: RPL chain cycle at page %d", id)
		}
		if uint64(len(headFirst)) > maxSegs {
			return nil, fmt.Errorf("pager: RPL chain exceeds bound %d (likely cycle)", maxSegs)
		}
		visited[id] = struct{}{}
		buf := p.Page(id)
		seg, ok := page.DecodeRPLSegment(buf, p.cfg)
		if !ok {
			return nil, fmt.Errorf("pager: RPL segment at page %d malformed", id)
		}
		if seg.OlderSegment == id {
			return nil, fmt.Errorf("pager: RPL segment at page %d is self-referential", id)
		}
		headFirst = append(headFirst, RPLSegmentRef{
			PageID: id,
			TxnID:  seg.TxnID,
			Count:  uint32(len(seg.PageIDs)),
		})
		id = seg.OlderSegment
	}
	// Reverse: head-first → tail-first.
	for i, j := 0, len(headFirst)-1; i < j; i, j = i+1, j-1 {
		headFirst[i], headFirst[j] = headFirst[j], headFirst[i]
	}
	return headFirst, nil
}


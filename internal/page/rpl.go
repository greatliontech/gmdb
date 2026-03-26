package page

// RPL segment pages store retired page IDs grouped by transaction. Each
// segment has a single TxnID and a linked-list pointer to the older segment.
//
// Byte layout after the 8-byte page header:
//
//	 8..15:  TxnID          uint64
//	16..23:  OlderSegment   uint64 (page ID of next older segment, 0 = tail)
//	24..25:  EntryCount     uint16
//	26..31:  Padding        (6 bytes)
//	32..:    PageID array   []uint64

// RPL segment header offsets (relative to start of page).
const (
	rplOffTxnID        = headerSize      // 8
	rplOffOlderSegment = rplOffTxnID + 8 // 16
	rplOffEntryCount   = rplOffOlderSegment + 8 // 24
	rplOffPadding      = rplOffEntryCount + 2   // 26
	rplDataStart       = rplOffPadding + 6      // 32
)

// RPLSegmentCapacity returns the maximum number of PageID entries for the
// given PageConfig.
func RPLSegmentCapacity(cfg PageConfig) int {
	avail := cfg.ContentEnd() - rplDataStart
	return avail / 8
}

// RPLSegmentReader provides zero-copy read access to an RPL segment page.
type RPLSegmentReader struct {
	buf []byte
	cfg PageConfig
}

// NewRPLSegmentReader wraps buf as an RPL segment page reader.
func NewRPLSegmentReader(buf []byte, cfg PageConfig) RPLSegmentReader {
	return RPLSegmentReader{buf: buf, cfg: cfg}
}

// TxnID returns the segment's transaction ID.
func (r RPLSegmentReader) TxnID() uint64 {
	return le.Uint64(r.buf[rplOffTxnID:])
}

// OlderSegment returns the page ID of the next older segment (0 = this is tail).
func (r RPLSegmentReader) OlderSegment() uint64 {
	return le.Uint64(r.buf[rplOffOlderSegment:])
}

// EntryCount returns the number of PageID entries in this segment.
func (r RPLSegmentReader) EntryCount() int {
	return int(le.Uint16(r.buf[rplOffEntryCount:]))
}

// PageID returns the i-th retired page ID.
func (r RPLSegmentReader) PageID(i int) uint64 {
	off := rplDataStart + i*8
	return le.Uint64(r.buf[off:])
}

// RPLSegmentBuilder constructs an RPL segment page in a []byte buffer.
type RPLSegmentBuilder struct {
	buf   []byte
	cfg   PageConfig
	count int
	cap   int
}

// NewRPLSegmentBuilder initializes a builder for writing into buf.
func NewRPLSegmentBuilder(buf []byte, cfg PageConfig) *RPLSegmentBuilder {
	// Zero the segment header area (padding included).
	clear(buf[headerSize:rplDataStart])
	return &RPLSegmentBuilder{
		buf: buf,
		cfg: cfg,
		cap: RPLSegmentCapacity(cfg),
	}
}

// SetTxnID writes the transaction ID.
func (b *RPLSegmentBuilder) SetTxnID(txnID uint64) {
	le.PutUint64(b.buf[rplOffTxnID:], txnID)
}

// SetOlderSegment writes the older segment link.
func (b *RPLSegmentBuilder) SetOlderSegment(pageID uint64) {
	le.PutUint64(b.buf[rplOffOlderSegment:], pageID)
}

// AddPageID appends a retired page ID. Returns false if at capacity.
func (b *RPLSegmentBuilder) AddPageID(pageID uint64) bool {
	if b.count >= b.cap {
		return false
	}
	off := rplDataStart + b.count*8
	le.PutUint64(b.buf[off:], pageID)
	b.count++
	return true
}

// Finish writes the page header and entry count. Returns the number of entries.
func (b *RPLSegmentBuilder) Finish() uint16 {
	count := uint16(b.count)
	WriteHeader(b.buf, TypeRPLSegment, 0, count, 0)
	le.PutUint16(b.buf[rplOffEntryCount:], count)
	return count
}

// Count returns the current number of entries added.
func (b *RPLSegmentBuilder) Count() int {
	return b.count
}

// Capacity returns the maximum number of entries this segment can hold.
func (b *RPLSegmentBuilder) Capacity() int {
	return b.cap
}

package pager

import (
	"fmt"

	"github.com/greatliontech/gmdb/internal/page"
)

// Retired Page Log segment-page layout, per free-space.md §Retired Page Log.
//
// Layout:
//
//	  0..7:   Common 8-byte page header (Type = page.TypeRPLSegment, Count = N entries)
//	  8..15:  TxnID         uint64  — transaction that retired these pages
//	 16..23:  OlderSegment  uint64  — page ID of the next older segment (0 only
//	          on a never-reclaimed original tail; the authoritative tail is
//	          meta.RPLTailPage — see free-space.md §Retired Page Log)
//	 24..24+8N: PageID array (N uint64 entries)
//
// Total per-segment overhead: 24 bytes. The page-header Count field
// carries the entry count — no separate EntryCount field exists. With
// PageChecksum enabled, the last 8 bytes of the page are the xxhash64
// footer, costing one entry slot.

// RPL segment field offsets.
const (
	rplOffTxnID        = 8
	rplOffOlderSegment = 16
	RPLHeaderSize      = 24 // common header + TxnID + OlderSegment
)

// RPLEntriesPerSegment returns the maximum number of PageID entries that
// fit in a single RPL segment for the given page configuration. Per
// free-space.md §Retired Page Log: 509 at 4 KB without checksum, 508 at
// 4 KB with checksum.
//
// Panics if cfg is invalid (per the package's "Config must be Validated
// at boundaries" rule).
func RPLEntriesPerSegment(cfg page.Config) int {
	cfg.MustValidate()
	usable := int(cfg.PageSize) - RPLHeaderSize
	if cfg.PageChecksum {
		usable -= page.FooterSize
	}
	return usable / 8
}

// RPLSegment is the decoded view of one RPL segment page. PageIDs is a
// snapshot copy — callers may mutate it independently of the source buffer.
type RPLSegment struct {
	TxnID        uint64
	OlderSegment uint64
	PageIDs      []uint64
}

// EntryCount returns len(PageIDs). Convenience for callers that want the
// segment's count without recomputing.
func (s RPLSegment) EntryCount() int { return len(s.PageIDs) }

// DecodeRPLSegment reads an RPL segment page from buf and returns the
// decoded view. The boolean return is the structural-validity signal:
// false means the buffer is malformed (wrong page type, entry count out
// of range, or buf too short for cfg.PageSize). Callers must treat false
// as corruption.
//
// Does NOT verify the xxhash64 footer — every caller that reads
// segment pages via a raw (non-verifying) accessor must verify the
// footer itself (VerifyPageFooter, when PageChecksum is enabled)
// before trusting the decoded view, per checksums.md §Verification.
func DecodeRPLSegment(buf []byte, cfg page.Config) (RPLSegment, bool) {
	cfg.MustValidate()
	if len(buf) < int(cfg.PageSize) {
		return RPLSegment{}, false
	}
	typ, _, count, _ := page.ReadHeader(buf)
	if typ != page.TypeRPLSegment {
		return RPLSegment{}, false
	}
	if int(count) > RPLEntriesPerSegment(cfg) {
		return RPLSegment{}, false
	}
	txnID := le.Uint64(buf[rplOffTxnID:])
	older := le.Uint64(buf[rplOffOlderSegment:])
	ids := make([]uint64, count)
	for i := range ids {
		ids[i] = le.Uint64(buf[RPLHeaderSize+i*8:])
	}
	return RPLSegment{TxnID: txnID, OlderSegment: older, PageIDs: ids}, true
}

// EncodeRPLSegment writes an RPL segment into buf. The caller is
// responsible for writing the xxhash64 footer (via page.WritePageFooter) when
// PageChecksum is enabled, after EncodeRPLSegment returns. Padding and
// the unused entry tail (between the last entry and ContentEnd) are
// zeroed.
//
// Panics if cfg is invalid, if buf is shorter than cfg.PageSize, or if
// len(pageIDs) > RPLEntriesPerSegment(cfg). All three are programming
// errors — wrong-size buf in particular would land mid-write without
// the upfront bounds check.
func EncodeRPLSegment(buf []byte, cfg page.Config, txnID, olderSegment uint64, pageIDs []uint64) {
	cfg.MustValidate()
	if len(buf) < int(cfg.PageSize) {
		panic(fmt.Sprintf("page: EncodeRPLSegment buf len %d < PageSize %d", len(buf), cfg.PageSize))
	}
	if max := RPLEntriesPerSegment(cfg); len(pageIDs) > max {
		panic(fmt.Sprintf("page: RPL segment overflow: %d entries > capacity %d", len(pageIDs), max))
	}
	count := uint16(len(pageIDs))
	page.WriteHeader(buf, page.TypeRPLSegment, count, 0)
	le.PutUint64(buf[rplOffTxnID:], txnID)
	le.PutUint64(buf[rplOffOlderSegment:], olderSegment)
	for i, id := range pageIDs {
		le.PutUint64(buf[RPLHeaderSize+i*8:], id)
	}
	// Zero the unused tail up to ContentEnd (the footer slot, if any,
	// is written by the caller after this returns).
	clear(buf[RPLHeaderSize+len(pageIDs)*8 : cfg.ContentEnd()])
}

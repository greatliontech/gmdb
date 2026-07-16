package page

import (
	"fmt"

	"github.com/zeebo/xxh3"
)

// Overflow-run layout per page-formats.md §Overflow Page:
//
//	Head page of run (PageChecksum enabled):
//	+---------------------------+ offset 0
//	| Page Header (8 bytes)     | Type=TypeOverflow, AdditionalPages=N
//	+---------------------------+ offset 8
//	| Whole-run digest (8 bytes)| XXH3-64 over the FULL content range
//	+---------------------------+ offset 16
//	| Extent bytes ...          | through the end of the page
//	+---------------------------+
//
//	Follower pages (N of them): pure extent bytes, offset 0 through
//	PageSize — no header, no footer, no digest.
//
// With PageChecksum disabled the digest field is absent and extent
// bytes start at head offset 8 (checksums.md §Overflow-Run Digest).
//
// The extent is ONE contiguous byte range: nothing interrupts it
// between the head's content start and the last follower's end, which
// is what lets a committed overflow value be returned as a single
// borrowed mmap slice (api-surface.md §Byte Slice Ownership). Run
// pages carry NO per-page checksum footers; the whole-run digest is
// the run's entire integrity cover. It is computed over the full
// content range — head content start through the last follower's
// end, the range AdditionalPages alone determines — never over the
// extent length, which lives only in the referencing cell (TotalLen
// for values, KeyTotalLen-T for key extents). Slack bytes past the
// extent length are zero on write, unconditionally, so a run image is
// a pure function of its extent bytes.

// OverflowDigestSize is the byte width of the head-resident whole-run
// digest (present only when PageChecksum is enabled).
const OverflowDigestSize = 8

// overflowDigestOff is the head-page offset of the whole-run digest,
// immediately after the 8-byte page header.
const overflowDigestOff = HeaderSize

// OverflowHeadContentStart returns the head-page offset at which
// extent bytes begin: after the header and, when PageChecksum is
// enabled, the whole-run digest.
func OverflowHeadContentStart(cfg Config) int {
	cfg.MustValidate()
	if cfg.PageChecksum {
		return HeaderSize + OverflowDigestSize
	}
	return HeaderSize
}

// OverflowFirstPageCapacity returns the extent-byte capacity of the
// head page in an overflow run: PageSize minus the header and the
// optional whole-run digest. Run pages carry no per-page footer.
func OverflowFirstPageCapacity(cfg Config) int {
	return int(cfg.PageSize) - OverflowHeadContentStart(cfg)
}

// OverflowFollowerCapacity returns the extent-byte capacity of a
// follower (non-head) page in an overflow run: the full PageSize.
// Followers carry no header, no footer, and no digest.
func OverflowFollowerCapacity(cfg Config) int {
	cfg.MustValidate()
	return int(cfg.PageSize)
}

// OverflowRunLength returns the number of pages (1 + AdditionalPages)
// required to store an extent of `valLen` bytes. Used by the allocator
// to size the contiguous run.
func OverflowRunLength(cfg Config, valLen uint64) uint32 {
	cfg.MustValidate()
	first := uint64(OverflowFirstPageCapacity(cfg))
	if valLen <= first {
		return 1
	}
	follower := uint64(OverflowFollowerCapacity(cfg))
	remaining := valLen - first
	// ceil(remaining / follower)
	return 1 + uint32((remaining+follower-1)/follower)
}

// OverflowRunLength64 is OverflowRunLength without the uint32 truncation:
// it returns the page count as a uint64. The write path stores values
// whose run length always fits a uint32, so OverflowRunLength suffices
// there; the READ/validation path must use this form, because a forged
// on-disk TotalLen can imply a run length that overflows uint32 and
// truncates to a small value — making a naive run-vs-extent guard pass
// while the TotalLen-sized allocation is enormous (checksums.md §Structural and Allocation Bounds). Callers
// bound the returned count against the file-resident extent before
// trusting TotalLen for any allocation.
func OverflowRunLength64(cfg Config, valLen uint64) uint64 {
	cfg.MustValidate()
	first := uint64(OverflowFirstPageCapacity(cfg))
	if valLen <= first {
		return 1
	}
	follower := uint64(OverflowFollowerCapacity(cfg))
	remaining := valLen - first
	return 1 + (remaining+follower-1)/follower
}

// EncodeOverflowRun writes value into a contiguous run of pages
// starting at pages[0]: head header, whole-run digest (when
// PageChecksum is enabled), and the extent bytes. pages MUST have
// exactly OverflowRunLength entries, each a page-sized buffer. Every
// page is cleared first, so slack past the extent is zero on write
// regardless of checksum setting. No per-page footers are applied —
// run pages are exempt from the commit footer pass.
//
// Returns an error if pages's length doesn't match the computed run
// length or if any page buffer is the wrong size — both are caller
// bugs in the allocator.
func EncodeOverflowRun(pages [][]byte, cfg Config, value []byte) error {
	cfg.MustValidate()
	want := int(OverflowRunLength(cfg, uint64(len(value))))
	if len(pages) != want {
		return fmt.Errorf("page: EncodeOverflowRun got %d pages, want %d for %d-byte value",
			len(pages), want, len(value))
	}
	for i, p := range pages {
		if len(p) != int(cfg.PageSize) {
			return fmt.Errorf("page: EncodeOverflowRun page %d len %d != PageSize %d",
				i, len(p), cfg.PageSize)
		}
		clear(p)
	}
	// Head page: header (+ digest slot) + extent prefix.
	WriteHeader(pages[0], TypeOverflow, 0, uint32(want-1))
	start := OverflowHeadContentStart(cfg)
	n := copy(pages[0][start:], value)
	value = value[n:]
	// Followers: pure extent bytes.
	for i := 1; i < want; i++ {
		m := copy(pages[i], value)
		value = value[m:]
	}
	if cfg.PageChecksum {
		// Whole-run digest over the FULL content range (checksums.md
		// §Overflow-Run Digest): head content start through the last
		// follower's end, slack included (zeroed above), so the run is
		// verifiable standalone from AdditionalPages alone. Streamed
		// because slab buffers are separate page-sized allocations;
		// the streaming Hasher produces the identical value to the
		// one-shot xxh3.Hash over the contiguous committed image.
		h := xxh3.New()
		_, _ = h.Write(pages[0][start:])
		for i := 1; i < want; i++ {
			_, _ = h.Write(pages[i])
		}
		le.PutUint64(pages[0][overflowDigestOff:], h.Sum64())
	}
	return nil
}

// DecodeOverflowFirstPage validates the head-page header and returns
// AdditionalPages (the count of follower pages in the run).
func DecodeOverflowFirstPage(buf []byte) (additional uint32, err error) {
	typ, _, _, additional := ReadHeader(buf)
	if typ != TypeOverflow {
		return 0, fmt.Errorf("page: DecodeOverflowFirstPage: type %d (want %d)", typ, TypeOverflow)
	}
	return additional, nil
}

// OverflowRunDigest computes the whole-run XXH3-64 digest of a
// CONTIGUOUS run image (head page followed immediately by its
// followers, (1+N)×PageSize bytes): one hash pass over the full
// content range from the head's content start.
func OverflowRunDigest(run []byte, cfg Config) uint64 {
	return xxh3.Hash(run[OverflowHeadContentStart(cfg):])
}

// StoredOverflowRunDigest returns the digest recorded in a run's head
// page. Only meaningful when PageChecksum is enabled.
func StoredOverflowRunDigest(head []byte) uint64 {
	return le.Uint64(head[overflowDigestOff:])
}

// SetOverflowRunDigest records the whole-run digest in a run's head
// page — for writers that stream the digest across separately-written
// pages (the bulk-load slab bypass) instead of encoding the run in
// one EncodeOverflowRun call. Only valid when PageChecksum is enabled
// (the field does not exist otherwise).
func SetOverflowRunDigest(head []byte, digest uint64) {
	le.PutUint64(head[overflowDigestOff:], digest)
}

// VerifyOverflowRun recomputes the whole-run digest of a contiguous
// run image and compares it to the head-resident stored digest.
// Always true when PageChecksum is disabled (no digest is stored).
func VerifyOverflowRun(run []byte, cfg Config) bool {
	if !cfg.PageChecksum {
		return true
	}
	return OverflowRunDigest(run, cfg) == StoredOverflowRunDigest(run)
}

// OverflowRunExtent returns the full-capacity extent range of a
// contiguous run image — head content start through the run's end.
// The caller slices it to the referencing cell's extent length
// (TotalLen / KeyTotalLen-T); bytes past that length are zero.
func OverflowRunExtent(run []byte, cfg Config) []byte {
	return run[OverflowHeadContentStart(cfg):]
}

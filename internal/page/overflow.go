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

// WriteOverflowRun encodes value as an overflow run and hands each
// completed page-sized buffer to write, one page at a time: followers
// first (indices 1..AdditionalPages, ascending), then the head
// (index 0) LAST — the head carries the whole-run XXH3-64 digest
// (when PageChecksum is enabled), which is only complete after every
// follower's bytes have been streamed through the hasher. write's
// buf argument is reused between calls; implementations must consume
// (write out / copy) it before returning. Trailing slack in the last
// follower (and past the head's extent prefix) is zero-filled, so
// the digest covers the full content range regardless of value
// length (checksums.md §Overflow-Run Digest).
//
// This is the ONE run-image ENCODER: online overflow-chain writes
// and the bulk-load path both produce their committed run bytes
// through it, so the digest and layout are computed in exactly one
// place. (Run relocation copies an existing digest-verified image
// byte-for-byte rather than re-encoding.) Memory is O(2 × PageSize)
// regardless of value size — run pages are never slab-resident
// (pager-slab.md §Slab Budget); the write callback pwrites each
// page directly.
//
// The first write error aborts the run and is returned; pages
// already written are the caller's to release (they are at fresh,
// unreferenced ids — bounded leakage on abort).
func WriteOverflowRun(cfg Config, value []byte, write func(idx uint32, buf []byte) error) error {
	cfg.MustValidate()
	runLen := OverflowRunLength(cfg, uint64(len(value)))
	// Head assembled in its own buffer and held back until the
	// streamed digest is complete. Freshly zeroed, so the region past
	// the copied prefix stays zero — slack is zero on write
	// (page-formats.md §Overflow Page).
	head := make([]byte, cfg.PageSize)
	WriteHeader(head, TypeOverflow, 0, runLen-1)
	start := OverflowHeadContentStart(cfg)
	off := copy(head[start:], value)
	h := xxh3.New()
	_, _ = h.Write(head[start:])
	// Followers: raw extent bytes, no header, no footer. clear(buf)
	// each iteration drops the previous page's content and zero-fills
	// the trailing slack of the final (partial) follower.
	buf := make([]byte, cfg.PageSize)
	followerCap := OverflowFollowerCapacity(cfg)
	for i := uint32(1); i < runLen; i++ {
		clear(buf)
		off += copy(buf[:followerCap], value[off:])
		_, _ = h.Write(buf)
		if err := write(i, buf); err != nil {
			return err
		}
	}
	if cfg.PageChecksum {
		SetOverflowRunDigest(head, h.Sum64())
	}
	return write(0, head)
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
// page. WriteOverflowRun calls it with the streamed digest once every
// follower has passed through the hasher; exposed for tests and any
// writer assembling a head out-of-band. Only valid when PageChecksum
// is enabled (the field does not exist otherwise).
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

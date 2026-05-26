package page

import "fmt"

// Overflow-page layout per page-formats.md §Overflow Page:
//
//	First page of run:
//	+-----------------------+ offset 0
//	| Page Header (8 bytes) | Type=TypeOverflow, AdditionalPages=N
//	+-----------------------+ offset 8
//	| Value bytes ...       | (PageSize - 8 - optional footer) bytes
//	+-----------------------+
//
//	Follower pages (N of them):
//	+-----------------------+ offset 0
//	| Value bytes ...       | (PageSize - optional footer) bytes
//	+-----------------------+
//
// Total capacity for 1 + N pages: (PageSize - 8) + N * PageSize
// (subtract another 8 per page for the footer when PageChecksum is
// enabled).

// OverflowFirstPageCapacity returns the value-byte capacity of the
// first page in an overflow run: PageSize - HeaderSize - optional
// footer.
func OverflowFirstPageCapacity(cfg Config) int {
	cfg.mustValidate()
	return cfg.ContentEnd() - HeaderSize
}

// OverflowFollowerCapacity returns the value-byte capacity of a
// follower (non-first) page in an overflow run: PageSize - optional
// footer. Follower pages carry no header.
func OverflowFollowerCapacity(cfg Config) int {
	cfg.mustValidate()
	return cfg.ContentEnd()
}

// OverflowRunLength returns the number of pages (1 + AdditionalPages)
// required to store a value of `valLen` bytes. Used by the allocator
// to size the contiguous run.
func OverflowRunLength(cfg Config, valLen uint64) uint32 {
	cfg.mustValidate()
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
// while the TotalLen-sized allocation is enormous (Inv-RV4). Callers
// bound the returned count against the file-resident extent before
// trusting TotalLen for any allocation.
func OverflowRunLength64(cfg Config, valLen uint64) uint64 {
	cfg.mustValidate()
	first := uint64(OverflowFirstPageCapacity(cfg))
	if valLen <= first {
		return 1
	}
	follower := uint64(OverflowFollowerCapacity(cfg))
	remaining := valLen - first
	return 1 + (remaining+follower-1)/follower
}

// EncodeOverflowRun writes value into a contiguous run of pages
// starting at pages[0]. pages MUST have exactly OverflowRunLength
// entries, each a page-sized buffer. The caller is responsible for
// applying per-page checksum footers via WritePageFooter at commit
// time (see commit.go's pwrite path).
//
// Returns an error if pages's length doesn't match the computed run
// length or if any page buffer is the wrong size — both are caller
// bugs in the allocator.
func EncodeOverflowRun(pages [][]byte, cfg Config, value []byte) error {
	cfg.mustValidate()
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
	// First page: header + value prefix.
	WriteHeader(pages[0], TypeOverflow, 0, uint32(want-1))
	firstCap := OverflowFirstPageCapacity(cfg)
	n := copy(pages[0][HeaderSize:HeaderSize+firstCap], value)
	value = value[n:]
	// Followers: pure value bytes (no header).
	followerCap := OverflowFollowerCapacity(cfg)
	for i := 1; i < want; i++ {
		m := copy(pages[i][:followerCap], value)
		value = value[m:]
	}
	return nil
}

// DecodeOverflowFirstPage validates the first-page header and
// returns AdditionalPages (the count of follower pages in the run).
func DecodeOverflowFirstPage(buf []byte) (additional uint32, err error) {
	typ, _, _, additional := ReadHeader(buf)
	if typ != TypeOverflow {
		return 0, fmt.Errorf("page: DecodeOverflowFirstPage: type %d (want %d)", typ, TypeOverflow)
	}
	return additional, nil
}

// AssembleOverflowValue concatenates the value bytes from a 1+N-page
// overflow run into the caller-provided dst slice. dst must be
// pre-sized to totalLen — the caller knows the length from the leaf
// reference's TotalLen field.
//
// pages must be the run in order: pages[0] = first (header-bearing),
// pages[1..N] = followers. Each must be a page-sized buffer.
//
// Returns the number of bytes written (= min(totalLen, run
// capacity)). Caller error on a mismatched dst size surfaces as a
// short read.
func AssembleOverflowValue(pages [][]byte, cfg Config, dst []byte) (int, error) {
	cfg.mustValidate()
	if len(pages) == 0 {
		return 0, fmt.Errorf("page: AssembleOverflowValue empty pages slice")
	}
	additional, err := DecodeOverflowFirstPage(pages[0])
	if err != nil {
		return 0, err
	}
	if len(pages) != int(additional)+1 {
		return 0, fmt.Errorf("page: AssembleOverflowValue got %d pages, header says %d",
			len(pages), int(additional)+1)
	}
	firstCap := OverflowFirstPageCapacity(cfg)
	n := copy(dst, pages[0][HeaderSize:HeaderSize+firstCap])
	followerCap := OverflowFollowerCapacity(cfg)
	for i := 1; i < len(pages); i++ {
		n += copy(dst[n:], pages[i][:followerCap])
	}
	return n, nil
}

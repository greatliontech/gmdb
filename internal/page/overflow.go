package page

// Overflow pages store large values as contiguous page runs. The first page
// has the standard 8-byte header (with AdditionalPages set to the follower
// count). Follower pages have no header — they are entirely value data.
// When PageChecksum is enabled, each page carries its own CRC32C footer.

// OverflowConfig holds precomputed capacity values for overflow pages.
type OverflowConfig struct {
	cfg               PageConfig
	firstPageCapacity int // usable data bytes in the first overflow page
	followerCapacity  int // usable data bytes in a follower page
}

// NewOverflowConfig creates an OverflowConfig for the given PageConfig.
func NewOverflowConfig(cfg PageConfig) OverflowConfig {
	firstCap := int(cfg.PageSize) - HeaderSize
	followerCap := int(cfg.PageSize)
	if cfg.PageChecksum {
		firstCap -= CRC32Size
		followerCap -= CRC32Size
	}
	return OverflowConfig{
		cfg:               cfg,
		firstPageCapacity: firstCap,
		followerCapacity:  followerCap,
	}
}

// FirstPageCapacity returns the usable data bytes in the first overflow page.
func (o OverflowConfig) FirstPageCapacity() int {
	return o.firstPageCapacity
}

// FollowerPageCapacity returns the usable data bytes in a follower page.
func (o OverflowConfig) FollowerPageCapacity() int {
	return o.followerCapacity
}

// PagesNeeded returns the total number of pages (first + followers) needed
// to store totalLen bytes of value data.
func (o OverflowConfig) PagesNeeded(totalLen int) int {
	if totalLen <= o.firstPageCapacity {
		return 1
	}
	remaining := totalLen - o.firstPageCapacity
	return 1 + (remaining+o.followerCapacity-1)/o.followerCapacity
}

// ReadFirstPage returns the value data from the first overflow page.
// The returned slice is borrowed from buf.
func (o OverflowConfig) ReadFirstPage(buf []byte) []byte {
	end := o.cfg.ContentEnd()
	return buf[HeaderSize:end]
}

// ReadFollowerPage returns the value data from a follower page.
// The returned slice is borrowed from buf.
func (o OverflowConfig) ReadFollowerPage(buf []byte) []byte {
	return buf[:o.followerCapacity]
}

// WriteFirstPage writes the page header and value data into the first
// overflow page buf. additionalPages is the number of follower pages.
// Returns the number of value bytes written.
func (o OverflowConfig) WriteFirstPage(buf []byte, additionalPages uint32, data []byte) int {
	WriteHeader(buf, TypeOverflow, 0, 0, additionalPages)
	end := o.cfg.ContentEnd()
	n := copy(buf[HeaderSize:end], data)
	return n
}

// WriteFollowerPage writes value data into a follower page buf (no header).
// Returns the number of value bytes written.
func (o OverflowConfig) WriteFollowerPage(buf []byte, data []byte) int {
	return copy(buf[:o.followerCapacity], data)
}

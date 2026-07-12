package gmdb

import (
	"fmt"

	"github.com/greatliontech/gmdb/internal/pager"
)

// FileFormat controls the database file's size bounds and growth/shrink
// behaviour. All sizes are in BYTES and must be multiples of PageSize.
// Lower (MinSize), GrowStep, and ShrinkThreshold are mutable at runtime via
// Tx.SetFileFormat; Upper (MaxSize) is immutable after creation. See
// docs/specs/file-format.md.
type FileFormat struct {
	// Lower is the minimum file size in bytes; the file never shrinks below.
	Lower uint64
	// Upper is the maximum file size in bytes — immutable after creation
	// (it determines the mmap reservation and bitmap size).
	Upper uint64
	// GrowStep is the byte increment the file grows by when extending
	// (0 ⇒ grow by exact need).
	GrowStep uint64
	// ShrinkThreshold is the trailing unused bytes that must accumulate
	// before the file is shrunk (0 ⇒ never shrink).
	ShrinkThreshold uint64
}

// SetFileFormat updates the mutable file-format parameters — Lower (MinSize),
// GrowStep, and ShrinkThreshold. Upper (MaxSize) is immutable: SetFileFormat
// returns ErrInvalidOptions if f.Upper differs from the current MaxSize
// (changing it would shift the bitmap region and every page offset —
// file-format.md §MaxSize is immutable). All sizes must be multiples of
// PageSize, and Lower must cover the metas + bitmap and not exceed MaxSize.
//
// The change is persisted atomically with this write transaction (discarded on
// Rollback) and takes effect from the NEXT transaction: the committed meta
// carries the new values, and the next Begin reloads the pager's growth/shrink
// state from it. Only valid on a write transaction.
func (tx *Tx) SetFileFormat(f FileFormat) error {
	if err := tx.requireOpen(true); err != nil {
		return err
	}
	ps := uint64(tx.prevMeta.PageSize)
	for _, s := range [...]struct {
		name string
		v    uint64
	}{{"Lower", f.Lower}, {"Upper", f.Upper}, {"GrowStep", f.GrowStep}, {"ShrinkThreshold", f.ShrinkThreshold}} {
		if s.v%ps != 0 {
			return fmt.Errorf("%w: FileFormat.%s %d is not a multiple of PageSize %d", ErrInvalidOptions, s.name, s.v, ps)
		}
	}
	if f.Upper != tx.prevMeta.MaxSize*ps {
		return fmt.Errorf("%w: FileFormat.Upper %d != current MaxSize %d bytes (MaxSize is immutable)", ErrInvalidOptions, f.Upper, tx.prevMeta.MaxSize*ps)
	}
	minPages := f.Lower / ps
	floorPages := uint64(2) + uint64(tx.prevMeta.BitmapPages)
	if minPages < floorPages {
		return fmt.Errorf("%w: FileFormat.Lower %d pages is below the floor of %d (2 metas + %d bitmap pages)", ErrInvalidOptions, minPages, floorPages, tx.prevMeta.BitmapPages)
	}
	if minPages > tx.prevMeta.MaxSize {
		return fmt.Errorf("%w: FileFormat.Lower %d pages exceeds MaxSize %d pages", ErrInvalidOptions, minPages, tx.prevMeta.MaxSize)
	}
	tx.pendingFileFormat = &pager.MetaFileFormat{
		MinSize:         minPages,
		GrowStep:        f.GrowStep / ps,
		ShrinkThreshold: f.ShrinkThreshold / ps,
	}
	return nil
}

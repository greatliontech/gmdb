package pager

import "github.com/thegrumpylion/gmdb/internal/bitmap"

// bitmapForOpen is the concrete type returned by bitmapWrap. It is a
// type alias to keep init.go free of the bitmap import in its function
// signatures while still letting Open() construct one.
type bitmapForOpen = bitmap.Bitmap

// bitmapWrap forwards to bitmap.New. The wrapper exists so the cross-
// package construction site is co-located with the bitmap import and
// init.go can stay free of bitmap symbols in its signatures.
func bitmapWrap(detail []byte, pageSize uint32, bitmapPages uint32, totalPages uint64) *bitmapForOpen {
	return bitmap.New(detail, pageSize, bitmapPages, totalPages)
}

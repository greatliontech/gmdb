// Package bitmap implements the gmdb allocation bitmap as a pure data
// structure: a two-level bitset (page-aligned detail level backed by []byte,
// and an in-memory summary backed by []uint64) with bit set/clear,
// contiguous-run search via math/bits intrinsics, LIFO hint tracking, and
// dirty-page tracking.
//
// The package performs no I/O and owns no file handles. The pager owns the
// on-disk bitmap pages and is responsible for materialising the detail
// level into memory at Open and applying dirty changes at commit; this
// package only manipulates the in-memory representation.
package bitmap

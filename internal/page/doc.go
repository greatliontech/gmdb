// Package page implements pure byte-slice encoders and decoders for every
// gmdb on-disk page format: the 8-byte page header, the meta page, the RPL
// segment, the branch page (prefix-truncated separators), the prefix-
// compressed leaf page (per-page RestartInterval), the overflow page header,
// the set-keyspace subpage, and the xxhash64 footer.
//
// The package operates strictly on []byte: it owns no file handles, performs
// no I/O, and depends on no OS facilities. Multi-byte integers are
// little-endian throughout (binary.LittleEndian).
//
// Invariants enforced here are encoding-level. Higher-level invariants
// (commit atomicity, free-space consistency, MVCC reader pinning) are
// enforced in internal/pager and internal/bitmap.
package page

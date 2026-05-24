// Package page implements pure byte-slice encoders and decoders for every
// gmdb on-disk binary format: the 8-byte page header, the meta page, the
// RPL segment, the branch page (prefix-truncated separators), the leaf
// page in two variants (compressed with variable-size restart groups +
// prefix-compressed delta entries; uncompressed with full keys + a
// positional offset table — selected per Config.RestartGroupTarget), the
// overflow page header, the set-keyspace subpage, the 40-byte keyspace
// descriptor (stored as the value for a keyspace's entry in the keyspace
// B+tree per keyspaces.md §Keyspace Descriptor), and the xxhash64 footer.
//
// The leaf surface is exposed via LeafReader (variant-dispatching read
// path) and LeafBuilder (variant-dispatching write path); LeafIter
// supports bidirectional cursor iteration over either variant with
// scratch-buffer reuse across leaf transitions.
//
// The package operates strictly on []byte: it owns no file handles, performs
// no I/O, and depends on no OS facilities. Multi-byte integers are
// little-endian throughout (binary.LittleEndian).
//
// Invariants enforced here are encoding-level. Higher-level invariants
// (commit atomicity, free-space consistency, MVCC reader pinning) are
// enforced in internal/pager and internal/bitmap.
package page

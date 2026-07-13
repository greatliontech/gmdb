// Package page implements pure byte-slice encoders and decoders for the
// B+tree NODE formats and the shared wire/header base every persisted
// format builds on: the 8-byte page header + page-type registry, Config,
// the XXH3-64 footer, the branch page (prefix-truncated separators),
// the leaf page in two variants (compressed with variable-size restart
// groups + prefix-compressed delta entries; uncompressed with full keys
// + a positional offset table — selected per Config.RestartGroupTarget),
// the overflow page header, and the set-keyspace subpage.
//
// The pager-DOMAIN formats live with their owner: the meta page and the
// RPL segment in internal/pager (which alone reads and writes them);
// the keyspace-descriptor row format in the root package (a leaf VALUE
// payload, not a page format). All build on this package's header,
// Config, and footer base.
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

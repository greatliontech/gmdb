// Package page implements pure byte-slice encoders and decoders for the
// B+tree NODE formats and the shared wire/header base every persisted
// format builds on: the 8-byte page header + page-type registry, Config,
// the XXH3-64 footer, the plain branch page (full separators),
// the leaf page in three variants (interleaved compressed — restart
// groups with value bytes following each entry's key bytes; segregated
// compressed — the same restart-group key compression with a pure
// headers+keys entry stream and value bytes in a separate end-anchored
// region located by per-entry VOff; uncompressed — full keys + a
// positional offset table; selected per Config.RestartGroupTarget and
// Config.LeafLayout, dispatched on read strictly by the page type
// byte), the overflow page header, and the set-keyspace subpage.
//
// The segregated leaf is the engine default: searches and key-only
// scans touch a value-free region and full-page decode reads two dense
// regions instead of one interleaved stream — the density/latency
// winner of the sibling-engine layout spikes — while the interleaved
// variant remains the choice for write-heavy keyspaces, whose in-place
// value splices avoid the segregated value-region shift (the "splice
// premium": a value-touching splice moves the value bytes and VOff
// fields of every earlier entry).
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

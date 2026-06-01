package gmdb

// TxIndexes is the index-administration surface of a write transaction,
// returned by Tx.Indexes. Its operations address a keyspace by NAME
// rather than through an opened *Keyspace handle, because Rebuild is the
// recovery path after ErrIndexFingerprintMismatch: it runs while
// OpenKeyspace is still failing for that keyspace, so no writable
// *Keyspace handle exists at the point of call (indexing.md §Rebuild,
// §Recovery pattern after ErrIndexFingerprintMismatch). Drop shares the
// surface for symmetry.
//
// TxIndexes is a lightweight value bound to its parent Tx; it holds no
// state of its own and is cheap to obtain per call
// (tx.Indexes().Rebuild(...)). It carries the parent transaction's
// read-only / closed state: Rebuild and Drop return ErrReadOnly on a
// read-only transaction and ErrTxClosed on a closed one, exactly as the
// other write APIs do.
type TxIndexes struct {
	tx *Tx
}

// Indexes returns the index-administration surface for this write
// transaction. See TxIndexes.
func (tx *Tx) Indexes() TxIndexes {
	return TxIndexes{tx: tx}
}

// Rebuild drops and re-populates the named index using the supplied
// IndexDecl. Per indexing.md §Rebuild + §Recovery pattern after
// ErrIndexFingerprintMismatch:
//
//   - The previous index is preserved until commit; a mid-rebuild
//     failure leaves the old index intact.
//   - decl.Name must match an existing registry entry's stored
//     Name; mismatch returns ErrIndexNotFound.
//   - decl.Extract must be non-nil (ErrIndexExtractorRequired).
//   - On success the stored SchemaHash and Version are replaced
//     by decl's values — this is the canonical recovery path after
//     ErrIndexFingerprintMismatch (bypasses the open-time
//     fingerprint check).
//   - Existing rows are re-extracted via decl.Extract; for unique
//     indexes a duplicate output aborts the rebuild with
//     ErrIndexUniqueViolation and the existing registry entry is
//     unchanged.
//
// Handle invalidation (indexing.md §Handle Invalidation): every
// in-flight *Index iter on this name surfaces ErrCursorStale on the
// next yield. The handle stays usable — a re-iterate after the rebuild
// opens a fresh cursor on the new pinned.root.
//
// Errors:
//   - ErrKeyEmpty if keyspace or decl.Name is empty.
//   - ErrIndexExtractorRequired if decl.Extract is nil.
//   - ErrNotFound if the keyspace does not exist
//     (keyspace-management dimension).
//   - ErrIndexNotFound if the keyspace exists but decl.Name is
//     not in its registry (index-management dimension).
//   - ErrKeyspaceReserved if the keyspace name resolves to Kind=2.
//   - ErrIndexUniqueViolation on duplicate keys from the new
//     extractor.
//   - ErrReadOnly on a read-only transaction; ErrTxClosed on a
//     closed transaction.
func (ix TxIndexes) Rebuild(keyspace string, decl *IndexDecl) error {
	return ix.tx.rebuildIndex(keyspace, decl)
}

// Drop removes the named index entirely. Retires the index's internal
// Kind=2 keyspace pages and the registry entry; if the dropped index
// was the keyspace's last, resets desc.IndexRegistryRoot to 0 and
// retires the registry sub-tree (keyspaces.md invariant #7 entailed).
//
// Handle invalidation (indexing.md §Handle Invalidation): every
// previously-handed-out *Index handle for this (keyspace, name) pair
// becomes dead — subsequent Lookup/LookupKeys/Range/Prefix/Get/Stats
// return ErrIndexNotFound. An in-flight iter at the moment of the drop
// surfaces ErrCursorStale on the next yield, after which the handle
// stays permanently dead within the transaction.
//
// Errors mirror Rebuild: ErrKeyEmpty (keyspace or indexName empty),
// ErrNotFound (keyspace missing), ErrIndexNotFound (index name not in
// the registry), ErrKeyspaceReserved (Kind=2), ErrReadOnly / ErrTxClosed.
func (ix TxIndexes) Drop(keyspace, indexName string) error {
	return ix.tx.dropIndex(keyspace, indexName)
}

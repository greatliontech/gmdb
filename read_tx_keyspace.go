package gmdb

// Keyspace-read surface for snapshot read transactions.
//
// A *ReadTx pins a consistent MVCC snapshot (BeginRead / View) that
// never blocks the single writer (transactions.md §Read Transaction).
// These methods expose the keyspace data of that snapshot for reading:
// they hand back the same *Keyspace / *SetKeyspace handles the write
// path uses, backed by a read-only *Tx (writable=false) whose pager is
// the ReadTx's read-only mmap and whose keyspace root is the snapshot's.
// Every read op (Get, Cursor, All / Range / Prefix, Index lookups,
// Has / HasValue / CountValues, Stats) works unchanged; every mutator
// returns ErrReadOnly.
//
// This is gmdb's concurrent-read path: N readers each on their own
// snapshot, none blocking the writer or each other (the only contention
// is reader-slot acquisition at BeginRead).

// keyspaceTx lazily builds the read-only *Tx backing keyspace reads for
// this snapshot. Reuses the full *Tx keyspace surface: requireOpen(true)
// (every mutator) returns ErrReadOnly because writable=false, while
// requireOpen(false) (reads) passes; descriptor lookup and B+tree reads
// resolve against the snapshot's KeyspaceRoot through the read-only
// pager. Returns ErrTxClosed once the ReadTx is closed, ErrClosed if the
// DB is closed.
func (rtx *ReadTx) keyspaceTx() (*Tx, error) {
	if rtx.closed {
		return nil, ErrTxClosed
	}
	if rtx.db.closeGate.IsClosed() {
		return nil, ErrClosed
	}
	if rtx.ksTx == nil {
		rtx.ksTx = &Tx{
			db:           rtx.db,
			pgr:          rtx.pgr,
			prevMeta:     rtx.meta,
			writable:     false,
			keyspaceRoot: rtx.meta.KeyspaceRoot,
			numKeyspaces: rtx.meta.NumKeyspaces,
		}
	}
	return rtx.ksTx, nil
}

// OpenKeyspaceReadOnly opens an existing single-value keyspace (Kind=0)
// for reading through this snapshot. The returned handle observes the
// snapshot's consistent, immutable view; its reads (Get, Cursor,
// All / Range / Prefix, Index) never block the writer, and its mutators
// (Put, Delete, DeleteRange, ...) return ErrReadOnly.
//
// Errors mirror Tx.OpenKeyspaceReadOnly (ErrNotFound, ErrKeyEmpty,
// ErrKeyspaceKindMismatch, ErrKeyspaceReserved, ErrCorrupted), plus
// ErrTxClosed after the ReadTx closes and ErrClosed if the DB is closed.
//
// Not safe for concurrent use by multiple goroutines on one ReadTx; use
// one ReadTx (one BeginRead) per goroutine for concurrent reads.
func (rtx *ReadTx) OpenKeyspaceReadOnly(name string) (*Keyspace, error) {
	tx, err := rtx.keyspaceTx()
	if err != nil {
		return nil, err
	}
	return tx.OpenKeyspaceReadOnly(name)
}

// OpenSetKeyspaceReadOnly opens an existing set keyspace (Kind=1) for
// reading through this snapshot. Same read/ErrReadOnly contract as
// OpenKeyspaceReadOnly.
func (rtx *ReadTx) OpenSetKeyspaceReadOnly(name string) (*SetKeyspace, error) {
	tx, err := rtx.keyspaceTx()
	if err != nil {
		return nil, err
	}
	return tx.OpenSetKeyspaceReadOnly(name)
}

// ListKeyspaces returns the names of all user keyspaces (Kind=0 / Kind=1)
// visible in this snapshot. Engine-internal index keyspaces are filtered
// out (as on the write path).
func (rtx *ReadTx) ListKeyspaces() ([]string, error) {
	tx, err := rtx.keyspaceTx()
	if err != nil {
		return nil, err
	}
	return tx.ListKeyspaces()
}

// TxnID returns the committed transaction id this snapshot observes —
// the version the read transaction is pinned to. Useful for correlating
// a read against a known write (e.g. read-your-writes across a Commit).
// Returns 0 for a genesis (never-written) snapshot.
func (rtx *ReadTx) TxnID() uint64 { return rtx.meta.TxnID }

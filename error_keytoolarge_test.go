package gmdb

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

// TestErrKeyTooLargeSentinel verifies the documented public sentinel:
// a key too large even for an overflow-reference leaf entry surfaces as
// gmdb.ErrKeyTooLarge through both Put and BulkLoad (the internal
// btree.ErrKeyTooLarge is translated by mapBtreeErr). Before this wiring
// the symbol did not exist, so errors.Is(err, gmdb.ErrKeyTooLarge) could
// not even compile.
func TestErrKeyTooLargeSentinel(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	// A key larger than a whole page cannot fit in a single-entry leaf
	// even in the overflow-reference form (keys, unlike values, never
	// promote to an overflow chain) — a genuine oversize key.
	bigKey := bytes.Repeat([]byte("k"), 8000)

	putErr := db.Update(ctx, func(tx *Tx) error {
		ks, e := tx.CreateKeyspace("ks")
		if e != nil {
			return e
		}
		return ks.Put(bigKey, []byte("v"))
	})
	if !errors.Is(putErr, ErrKeyTooLarge) {
		t.Errorf("Put oversize key: got %v, want ErrKeyTooLarge", putErr)
	}

	oneBig := func(yield func([]byte, []byte) bool) { yield(bigKey, []byte("v")) }

	// Keyspace.BulkLoad (non-indexed).
	if e := db.Update(ctx, func(tx *Tx) error {
		ks, err := tx.CreateKeyspace("ks2")
		if err != nil {
			return err
		}
		_, err = ks.BulkLoad(oneBig)
		return err
	}); !errors.Is(e, ErrKeyTooLarge) {
		t.Errorf("Keyspace.BulkLoad oversize key: got %v, want ErrKeyTooLarge", e)
	}

	// NOTE: SetKeyspace.BulkLoad of an oversize set key surfaces a
	// DIFFERENT error (the bulk builder's errBulkEntryTooLarge, because
	// the set-key path does not pre-check the key the way the Keyspace
	// path's bulkLeafEntry does) — tracked separately under
	// setkeyspace-bulkload-oversize-key, out of scope for this sentinel.

	// Indexed Keyspace.BulkLoad (the indexed-path wrap).
	if e := db.Update(ctx, func(tx *Tx) error {
		decl := testDecl("by_b", "b")
		decl.Extract = firstByteExtract
		ks, err := tx.CreateKeyspace("idxks", decl)
		if err != nil {
			return err
		}
		_, err = ks.BulkLoad(oneBig)
		return err
	}); !errors.Is(e, ErrKeyTooLarge) {
		t.Errorf("indexed Keyspace.BulkLoad oversize key: got %v, want ErrKeyTooLarge", e)
	}
}

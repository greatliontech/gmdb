package gmdb

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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

	// SetKeyspace.BulkLoad (non-indexed): the set-key path now pre-checks
	// the set key (setBulk.flush) and the boundary translates via
	// mapBtreeErr, so an oversize set key surfaces the public sentinel
	// (was the internal errBulkEntryTooLarge).
	if e := db.Update(ctx, func(tx *Tx) error {
		sks, err := tx.CreateSetKeyspace("sks", nil)
		if err != nil {
			return err
		}
		_, err = sks.BulkLoad(oneBig)
		return err
	}); !errors.Is(e, ErrKeyTooLarge) {
		t.Errorf("SetKeyspace.BulkLoad oversize set key: got %v, want ErrKeyTooLarge", e)
	}

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

	// Indexed SetKeyspace.BulkLoad — the audited missing path: the
	// three boundary returns previously leaked the internal
	// btree/bulkload sentinels unmapped.
	setExtract := func(setKey, member []byte) []IndexEntry {
		return []IndexEntry{{Cols: [][]byte{member[:1]}}}
	}
	if e := db.Update(ctx, func(tx *Tx) error {
		sks, err := tx.CreateSetKeyspace("idxsks", nil,
			&IndexDecl{Name: "by_m", Columns: []IndexColumn{{Name: "m"}}, Extract: setExtract})
		if err != nil {
			return err
		}
		_, err = sks.BulkLoad(oneBig)
		return err
	}); !errors.Is(e, ErrKeyTooLarge) {
		t.Errorf("indexed SetKeyspace.BulkLoad oversize set key: got %v, want ErrKeyTooLarge", e)
	}

	// Oversize FIRST value of a set key (variable-size sets): bypasses
	// the promotion threshold by design (Put's genesis shape), reaches
	// the builder as a subpage cell too large for an empty leaf, and
	// must surface the public sentinel — the same input via Put maps
	// to ErrKeyTooLarge. Both the plain and indexed set paths.
	bigVal := bytes.Repeat([]byte("v"), 8000)
	oneBigVal := func(yield func([]byte, []byte) bool) { yield([]byte("k"), bigVal) }
	if e := db.Update(ctx, func(tx *Tx) error {
		sks, err := tx.CreateSetKeyspace("sksv", nil)
		if err != nil {
			return err
		}
		_, err = sks.BulkLoad(oneBigVal)
		return err
	}); !errors.Is(e, ErrKeyTooLarge) {
		t.Errorf("SetKeyspace.BulkLoad oversize first value: got %v, want ErrKeyTooLarge", e)
	}
	if e := db.Update(ctx, func(tx *Tx) error {
		sks, err := tx.CreateSetKeyspace("idxsksv", nil,
			&IndexDecl{Name: "by_m2", Columns: []IndexColumn{{Name: "m"}}, Extract: setExtract})
		if err != nil {
			return err
		}
		_, err = sks.BulkLoad(oneBigVal)
		return err
	}); !errors.Is(e, ErrKeyTooLarge) {
		t.Errorf("indexed SetKeyspace.BulkLoad oversize first value: got %v, want ErrKeyTooLarge", e)
	}

	// Oversize INDEX key produced by the extractor (limits.md subjects
	// index keys to the ordinary key maximum): the index-build
	// boundary must map the builder guard too — both indexed variants.
	hugeColExtract := func(_, _ []byte) []IndexEntry {
		return []IndexEntry{{Cols: [][]byte{bytes.Repeat([]byte("c"), 8000)}}}
	}
	if e := db.Update(ctx, func(tx *Tx) error {
		ks, err := tx.CreateKeyspace("idxkshuge",
			&IndexDecl{Name: "hg", Columns: []IndexColumn{{Name: "c"}}, Extract: hugeColExtract})
		if err != nil {
			return err
		}
		_, err = ks.BulkLoad(func(yield func([]byte, []byte) bool) { yield([]byte("k"), []byte("v")) })
		return err
	}); !errors.Is(e, ErrKeyTooLarge) {
		t.Errorf("indexed Keyspace.BulkLoad oversize index key: got %v, want ErrKeyTooLarge", e)
	}
	hugeMemberColExtract := func(_, _ []byte) []IndexEntry {
		return []IndexEntry{{Cols: [][]byte{bytes.Repeat([]byte("c"), 8000)}}}
	}
	if e := db.Update(ctx, func(tx *Tx) error {
		sks, err := tx.CreateSetKeyspace("idxskshuge", nil,
			&IndexDecl{Name: "hg2", Columns: []IndexColumn{{Name: "c"}}, Extract: hugeMemberColExtract})
		if err != nil {
			return err
		}
		_, err = sks.BulkLoad(func(yield func([]byte, []byte) bool) { yield([]byte("k"), []byte("m")) })
		return err
	}); !errors.Is(e, ErrKeyTooLarge) {
		t.Errorf("indexed SetKeyspace.BulkLoad oversize index key: got %v, want ErrKeyTooLarge", e)
	}

	// Put parity for the oversize single value (the contract the bulk
	// paths must match).
	if e := db.Update(ctx, func(tx *Tx) error {
		sks, err := tx.CreateSetKeyspace("sksput", nil)
		if err != nil {
			return err
		}
		_, err = sks.Put([]byte("k"), bigVal)
		return err
	}); !errors.Is(e, ErrKeyTooLarge) {
		t.Errorf("SetKeyspace.Put oversize value: got %v, want ErrKeyTooLarge", e)
	}
}

// TestKeyTooLargeDeterministicAtBound pins the split-safety key
// bound (limits.md §Maximum Key Size): every entry gate enforces the
// spec's two-full-separators-per-branch bound (~(PageSize-40)/2), so
// a key in the gap between leaf-entry fit and the spec bound fails
// ErrKeyTooLarge AT the operation, uniformly across Put and the bulk
// builders. Keys at the bound must work through real splits.
func TestKeyTooLargeDeterministicAtBound(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 512})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	// ~3000 bytes at 4 KiB: fits a single leaf entry (the old gate
	// accepted it) but two such separators cannot share a branch.
	gapKey := bytes.Repeat([]byte("g"), 3000)
	if e := db.Update(ctx, func(tx *Tx) error {
		ks, err := tx.CreateKeyspace("gap")
		if err != nil {
			return err
		}
		return ks.Put(gapKey, []byte("v"))
	}); !errors.Is(e, ErrKeyTooLarge) {
		t.Errorf("gap-key Put: got %v, want ErrKeyTooLarge (deterministic at the entry gate)", e)
	}

	// SetKeyspace top-level key (PutEntry path — H-shape from review:
	// an ungated set key was accepted by Put and then failed CopyTo's
	// gated rebuild) and set MEMBERS (bulk path) obey the same bound.
	if e := db.Update(ctx, func(tx *Tx) error {
		sks, err := tx.CreateSetKeyspace("sgap", nil)
		if err != nil {
			return err
		}
		_, err = sks.Put(gapKey, []byte("m"))
		return err
	}); !errors.Is(e, ErrKeyTooLarge) {
		t.Errorf("set gap-key Put: got %v, want ErrKeyTooLarge", e)
	}
	gapMember := bytes.Repeat([]byte("m"), 3000)
	if e := db.Update(ctx, func(tx *Tx) error {
		sks, err := tx.CreateSetKeyspace("smem", nil)
		if err != nil {
			return err
		}
		_, err = sks.BulkLoad(func(yield func([]byte, []byte) bool) {
			yield([]byte("k"), gapMember)
		})
		return err
	}); !errors.Is(e, ErrKeyTooLarge) {
		t.Errorf("set gap-member BulkLoad: got %v, want ErrKeyTooLarge", e)
	}
	if e := db.Update(ctx, func(tx *Tx) error {
		sks, err := tx.CreateSetKeyspace("smem2", nil)
		if err != nil {
			return err
		}
		_, err = sks.Put([]byte("k"), gapMember)
		return err
	}); !errors.Is(e, ErrKeyTooLarge) {
		t.Errorf("set gap-member Put: got %v, want ErrKeyTooLarge", e)
	}

	// A FixedValueSize above the member bound is rejected at
	// declaration — otherwise the keyspace would accept nothing (its
	// every correct-length Put failing the member gate forever).
	if e := db.Update(ctx, func(tx *Tx) error {
		_, err := tx.CreateSetKeyspace("fvsbig", &SetKeyspaceOptions{FixedValueSize: 3000})
		return err
	}); !errors.Is(e, ErrInvalidOptions) {
		t.Errorf("over-bound FixedValueSize declaration: got %v, want ErrInvalidOptions", e)
	}

	// At-bound keys (~1900 bytes, safely under (4096-40)/2) must
	// survive enough inserts to force branch splits.
	if err := db.Update(ctx, func(tx *Tx) error {
		ks, err := tx.CreateKeyspace("bound")
		if err != nil {
			return err
		}
		for i := range 60 {
			k := bytes.Repeat([]byte{byte('a' + i%26)}, 1900)
			k = append(k, byte(i))
			if err := ks.Put(k, []byte("v")); err != nil {
				return fmt.Errorf("at-bound Put %d: %w", i, err)
			}
		}
		return nil
	}); err != nil {
		t.Errorf("at-bound keys through splits: %v", err)
	}
}

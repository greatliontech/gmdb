package gmdb

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
)

// TestErrKeyTooLargeSentinel verifies the documented public sentinel
// on the surfaces where a size bound REMAINS after overflow-key cells
// (limits.md): set VALUES over the inline threshold (the set-keyspace
// surface has not adopted overflow-key members). Ordinary keys of any
// length store — TestOverThresholdKeysRoundTrip pins that side.
func TestErrKeyTooLargeSentinel(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	// Keys over the inline threshold take the overflow-key form on
	// EVERY entry path — Put, plain BulkLoad, set-key BulkLoad,
	// indexed BulkLoad — and round-trip (limits.md §Maximum Key Size).
	bigKey := bytes.Repeat([]byte("k"), 8000)

	if err := db.Update(ctx, func(tx *Tx) error {
		ks, e := tx.CreateKeyspace("ks")
		if e != nil {
			return e
		}
		return ks.Put(bigKey, []byte("v"))
	}); err != nil {
		t.Errorf("Put over-threshold key: %v", err)
	}
	if err := db.View(ctx, func(rtx *ReadTx) error {
		ks, e := rtx.OpenKeyspaceReadOnly("ks")
		if e != nil {
			return e
		}
		v, e := ks.Get(bigKey)
		if e != nil {
			return e
		}
		if !bytes.Equal(v, []byte("v")) {
			return fmt.Errorf("Get over-threshold key = %q, want %q", v, "v")
		}
		return nil
	}); err != nil {
		t.Errorf("read back over-threshold key: %v", err)
	}

	oneBig := func(yield func([]byte, []byte) bool) { yield(bigKey, []byte("v")) }

	if e := db.Update(ctx, func(tx *Tx) error {
		ks, err := tx.CreateKeyspace("ks2")
		if err != nil {
			return err
		}
		_, err = ks.BulkLoad(oneBig)
		return err
	}); e != nil {
		t.Errorf("Keyspace.BulkLoad over-threshold key: %v", e)
	}

	// Set-keyspace TOP-LEVEL keys share the ordinary key contract.
	if e := db.Update(ctx, func(tx *Tx) error {
		sks, err := tx.CreateSetKeyspace("sks", nil)
		if err != nil {
			return err
		}
		_, err = sks.BulkLoad(oneBig)
		return err
	}); e != nil {
		t.Errorf("SetKeyspace.BulkLoad over-threshold set key: %v", e)
	}

	setExtract := func(setKey, member []byte) []IndexEntry {
		return []IndexEntry{{Cols: [][]byte{member[:1]}}}
	}
	if e := db.Update(ctx, func(tx *Tx) error {
		decl := testDecl("by_b", "b")
		decl.Extract = firstByteExtract
		ks, err := tx.CreateKeyspace("idxks", decl)
		if err != nil {
			return err
		}
		_, err = ks.BulkLoad(oneBig)
		return err
	}); e != nil {
		t.Errorf("indexed Keyspace.BulkLoad over-threshold key: %v", e)
	}
	if e := db.Update(ctx, func(tx *Tx) error {
		sks, err := tx.CreateSetKeyspace("idxsks", nil,
			&IndexDecl{Name: "by_m", Columns: []IndexColumn{{Name: "m"}}, Extract: setExtract})
		if err != nil {
			return err
		}
		_, err = sks.BulkLoad(oneBig)
		return err
	}); e != nil {
		t.Errorf("indexed SetKeyspace.BulkLoad over-threshold set key: %v", e)
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

	// Over-threshold INDEX keys produced by the extractor share the
	// ordinary key contract (limits.md §Maximum Index Key Size): they
	// store as overflow-key cells in the index tree and the lookup
	// resolves them — both indexed variants.
	hugeCol := bytes.Repeat([]byte("c"), 8000)
	hugeColExtract := func(_, _ []byte) []IndexEntry {
		return []IndexEntry{{Cols: [][]byte{hugeCol}}}
	}
	if e := db.Update(ctx, func(tx *Tx) error {
		ks, err := tx.CreateKeyspace("idxkshuge",
			&IndexDecl{Name: "hg", Columns: []IndexColumn{{Name: "c"}}, Extract: hugeColExtract})
		if err != nil {
			return err
		}
		if _, err = ks.BulkLoad(func(yield func([]byte, []byte) bool) { yield([]byte("k"), []byte("v")) }); err != nil {
			return err
		}
		idx, err := ks.Index("hg")
		if err != nil {
			return err
		}
		var pks [][]byte
		for pk := range idx.LookupKeys([][]byte{hugeCol}) {
			pks = append(pks, bytes.Clone(pk))
		}
		if err := idx.Err(); err != nil {
			return err
		}
		if len(pks) != 1 || !bytes.Equal(pks[0], []byte("k")) {
			return fmt.Errorf("huge-index-key lookup = %q, want [k]", pks)
		}
		return nil
	}); e != nil {
		t.Errorf("indexed Keyspace over-threshold index key: %v", e)
	}
	if e := db.Update(ctx, func(tx *Tx) error {
		sks, err := tx.CreateSetKeyspace("idxskshuge", nil,
			&IndexDecl{Name: "hg2", Columns: []IndexColumn{{Name: "c"}}, Extract: func(_, _ []byte) []IndexEntry {
				return []IndexEntry{{Cols: [][]byte{hugeCol}}}
			}})
		if err != nil {
			return err
		}
		_, err = sks.BulkLoad(func(yield func([]byte, []byte) bool) { yield([]byte("k"), []byte("m")) })
		return err
	}); e != nil {
		t.Errorf("indexed SetKeyspace over-threshold index key: %v", e)
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

// TestKeyTooLargeDeterministicAtBound pins the storable-key contract
// at the inline-threshold boundary (limits.md §Maximum Key Size):
// keys just past the threshold — the range the old branch-budget gate
// rejected — store uniformly across Put and the set-key path and
// round-trip; set MEMBERS keep their inline bound (limits.md §Maximum
// Value Size (Set Keyspaces)) uniformly across Put and BulkLoad, and
// an over-threshold FixedValueSize is rejected at declaration. Keys
// near the threshold must survive real splits on both sides.
func TestKeyTooLargeDeterministicAtBound(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 512})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	// ~3000 bytes at 4 KiB: over the inline threshold (2010 with
	// checksums) — the old branch-budget gate rejected it; it now
	// stores as an overflow-key cell and reads back.
	gapKey := bytes.Repeat([]byte("g"), 3000)
	if e := db.Update(ctx, func(tx *Tx) error {
		ks, err := tx.CreateKeyspace("gap")
		if err != nil {
			return err
		}
		return ks.Put(gapKey, []byte("v"))
	}); e != nil {
		t.Errorf("gap-key Put: %v", e)
	}
	if e := db.View(ctx, func(rtx *ReadTx) error {
		ks, err := rtx.OpenKeyspaceReadOnly("gap")
		if err != nil {
			return err
		}
		v, err := ks.Get(gapKey)
		if err != nil {
			return err
		}
		if !bytes.Equal(v, []byte("v")) {
			return fmt.Errorf("gap-key Get = %q, want v", v)
		}
		return nil
	}); e != nil {
		t.Errorf("gap-key read back: %v", e)
	}

	// SetKeyspace top-level key (PutEntry path) shares the ordinary
	// key contract.
	if e := db.Update(ctx, func(tx *Tx) error {
		sks, err := tx.CreateSetKeyspace("sgap", nil)
		if err != nil {
			return err
		}
		_, err = sks.Put(gapKey, []byte("m"))
		return err
	}); e != nil {
		t.Errorf("set gap-key Put: %v", e)
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

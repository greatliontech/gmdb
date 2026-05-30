package gmdb

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"iter"
	"testing"

	"github.com/thegrumpylion/gmdb/internal/btree"
	"github.com/thegrumpylion/gmdb/internal/page"
)

// setData is one key with its sorted, unique value set.
type setData struct {
	key    []byte
	values [][]byte
}

// setSeq flattens sorted setData to a (key, value) stream in (key, value)
// lex order (the SetKeyspace.BulkLoad input contract).
func setSeq(data []setData) iter.Seq2[[]byte, []byte] {
	return func(yield func(k, v []byte) bool) {
		for _, d := range data {
			for _, v := range d.values {
				if !yield(d.key, v) {
					return
				}
			}
		}
	}
}

func totalMembers(data []setData) uint64 {
	var n uint64
	for _, d := range data {
		n += uint64(len(d.values))
	}
	return n
}

// genSetData builds numKeys keys; key i gets valuesFn(i) values, each an
// 8-byte zero-padded decimal (fixed-value-size friendly), sorted unique.
func genSetData(numKeys int, valuesFn func(i int) int) []setData {
	data := make([]setData, numKeys)
	for i := range numKeys {
		nv := valuesFn(i)
		vals := make([][]byte, nv)
		for j := range nv {
			vals[j] = fmt.Appendf(nil, "%08d", j)
		}
		data[i] = setData{key: fmt.Appendf(nil, "key%05d", i), values: vals}
	}
	return data
}

// verifySetKeyspace checks CountValues + HasValue for every member and a
// full ordered SetCursor scan, on a freshly reopened keyspace.
func verifySetKeyspace(t *testing.T, ks *SetKeyspace, data []setData) {
	t.Helper()
	for _, d := range data {
		got, err := ks.CountValues(d.key)
		if err != nil {
			t.Fatalf("CountValues(%q): %v", d.key, err)
		}
		if got != uint64(len(d.values)) {
			t.Fatalf("CountValues(%q) = %d, want %d", d.key, got, len(d.values))
		}
		for _, v := range d.values {
			has, err := ks.HasValue(d.key, v)
			if err != nil {
				t.Fatalf("HasValue(%q,%q): %v", d.key, v, err)
			}
			if !has {
				t.Fatalf("HasValue(%q,%q) = false, want true", d.key, v)
			}
		}
	}
	// Full ordered (key,value) scan.
	c := ks.Cursor()
	ki, vi := 0, 0
	var seen uint64
	for k, v := c.First(); k != nil; k, v = c.Next() {
		if ki >= len(data) {
			t.Fatalf("cursor overran keys at (%q,%q)", k, v)
		}
		for vi >= len(data[ki].values) { // advance to next key
			ki++
			vi = 0
			if ki >= len(data) {
				t.Fatalf("cursor overran at (%q,%q)", k, v)
			}
		}
		if !bytes.Equal(k, data[ki].key) || !bytes.Equal(v, data[ki].values[vi]) {
			t.Fatalf("scan (%q,%q), want (%q,%q)", k, v, data[ki].key, data[ki].values[vi])
		}
		vi++
		seen++
	}
	if err := c.Err(); err != nil {
		t.Fatalf("cursor Err: %v", err)
	}
	// Closes the trailing-drop gap: the scan must have consumed every
	// expected (key, value) pair, not stopped early.
	if want := totalMembers(data); seen != want {
		t.Fatalf("cursor scan yielded %d pairs, want %d", seen, want)
	}
}

func TestSetKeyspaceBulkLoadRoundTrip(t *testing.T) {
	type variant struct {
		name string
		fvs  int
	}
	for _, vr := range []variant{{"variable", 0}, {"fixed8", 8}} {
		t.Run(vr.name, func(t *testing.T) {
			ctx := context.Background()
			path := tmpPath(t)
			opts := Options{PageSize: 4096, MinSize: 16, MaxSize: 16384}

			// Mix: most keys are small (subpage); every 10th key has a
			// large set (nested tree). Values are 8 bytes (fixed-safe).
			data := genSetData(120, func(i int) int {
				if i%10 == 0 {
					return 400 // exceeds the subpage threshold → nested tree
				}
				return 1 + i%7
			})

			db := openWith(t, ctx, path, opts)
			tx, err := db.Begin(ctx)
			if err != nil {
				t.Fatalf("Begin: %v", err)
			}
			var sopts *SetKeyspaceOptions
			if vr.fvs != 0 {
				sopts = &SetKeyspaceOptions{FixedValueSize: vr.fvs}
			}
			ks, err := tx.CreateSetKeyspace("sets", sopts)
			if err != nil {
				t.Fatalf("CreateSetKeyspace: %v", err)
			}
			n, err := ks.BulkLoad(setSeq(data))
			if err != nil {
				t.Fatalf("BulkLoad: %v", err)
			}
			want := totalMembers(data)
			if n != want {
				t.Errorf("BulkLoad returned %d, want %d", n, want)
			}
			if ks.desc.Count != want {
				t.Errorf("desc.Count = %d, want %d", ks.desc.Count, want)
			}
			if err := tx.Commit(); err != nil {
				t.Fatalf("Commit: %v", err)
			}
			db.Close()

			db2 := openWith(t, ctx, path, opts)
			defer db2.Close()
			tx2, err := db2.Begin(ctx)
			if err != nil {
				t.Fatalf("re-Begin: %v", err)
			}
			defer tx2.Rollback()
			ks2, err := tx2.OpenSetKeyspace("sets")
			if err != nil {
				t.Fatalf("re-OpenSetKeyspace: %v", err)
			}
			verifySetKeyspace(t, ks2, data)
		})
	}
}

// TestSetKeyspaceBulkLoadStreamingPromotion drives one key with a very large
// value set (multi-level nested tree) to exercise the buffer→nested promotion
// streaming path.
func TestSetKeyspaceBulkLoadStreamingPromotion(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)
	opts := Options{PageSize: 4096, MinSize: 16, MaxSize: 16384}

	data := []setData{{key: []byte("hot"), values: nil}}
	for j := range 8000 {
		data[0].values = append(data[0].values, fmt.Appendf(nil, "m%07d", j))
	}

	db := openWith(t, ctx, path, opts)
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	ks, err := tx.CreateSetKeyspace("posts", nil)
	if err != nil {
		t.Fatalf("CreateSetKeyspace: %v", err)
	}
	n, err := ks.BulkLoad(setSeq(data))
	if err != nil {
		t.Fatalf("BulkLoad: %v", err)
	}
	if n != 8000 {
		t.Errorf("BulkLoad returned %d, want 8000", n)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	db.Close()

	db2 := openWith(t, ctx, path, opts)
	defer db2.Close()
	tx2, err := db2.Begin(ctx)
	if err != nil {
		t.Fatalf("re-Begin: %v", err)
	}
	defer tx2.Rollback()
	ks2, err := tx2.OpenSetKeyspace("posts")
	if err != nil {
		t.Fatalf("OpenSetKeyspace: %v", err)
	}
	cv, err := ks2.CountValues([]byte("hot"))
	if err != nil {
		t.Fatalf("CountValues: %v", err)
	}
	if cv != 8000 {
		t.Errorf("CountValues = %d, want 8000", cv)
	}
	verifySetKeyspace(t, ks2, data)
}

// TestSetKeyspaceBulkLoadDedup verifies duplicate (key,value) pairs are
// silently deduplicated and the count reflects distinct members.
func TestSetKeyspaceBulkLoadDedup(t *testing.T) {
	ctx := context.Background()
	db := openWith(t, ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	defer db.Close()
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()
	ks, err := tx.CreateSetKeyspace("sets", nil)
	if err != nil {
		t.Fatalf("CreateSetKeyspace: %v", err)
	}
	// k1 -> {a, a, b}  (a deduped); k2 -> {x}
	stream := []kv{
		{[]byte("k1"), []byte("a")},
		{[]byte("k1"), []byte("a")}, // dup
		{[]byte("k1"), []byte("b")},
		{[]byte("k2"), []byte("x")},
	}
	n, err := ks.BulkLoad(seqOf(stream))
	if err != nil {
		t.Fatalf("BulkLoad: %v", err)
	}
	if n != 3 {
		t.Errorf("BulkLoad returned %d, want 3 (one dup removed)", n)
	}
	if c, _ := ks.CountValues([]byte("k1")); c != 2 {
		t.Errorf("CountValues(k1) = %d, want 2", c)
	}
}

func TestSetKeyspaceBulkLoadErrors(t *testing.T) {
	ctx := context.Background()
	newKS := func(t *testing.T, fvs int) (*SetKeyspace, func()) {
		db := openWith(t, ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
		tx, err := db.Begin(ctx)
		if err != nil {
			t.Fatalf("Begin: %v", err)
		}
		var so *SetKeyspaceOptions
		if fvs != 0 {
			so = &SetKeyspaceOptions{FixedValueSize: fvs}
		}
		ks, err := tx.CreateSetKeyspace("sets", so)
		if err != nil {
			t.Fatalf("CreateSetKeyspace: %v", err)
		}
		return ks, func() { _ = tx.Rollback(); _ = db.Close() }
	}

	t.Run("non-empty", func(t *testing.T) {
		ks, done := newKS(t, 0)
		defer done()
		if _, err := ks.Put([]byte("k"), []byte("v")); err != nil {
			t.Fatalf("Put: %v", err)
		}
		if _, err := ks.BulkLoad(seqOf([]kv{{[]byte("a"), []byte("b")}})); !errors.Is(err, ErrBulkLoadNonEmpty) {
			t.Errorf("= %v, want ErrBulkLoadNonEmpty", err)
		}
	})
	t.Run("out-of-order-key", func(t *testing.T) {
		ks, done := newKS(t, 0)
		defer done()
		bad := []kv{{[]byte("k2"), []byte("a")}, {[]byte("k1"), []byte("a")}}
		if _, err := ks.BulkLoad(seqOf(bad)); !errors.Is(err, ErrBulkLoadOutOfOrder) {
			t.Errorf("= %v, want ErrBulkLoadOutOfOrder", err)
		}
	})
	t.Run("out-of-order-value", func(t *testing.T) {
		ks, done := newKS(t, 0)
		defer done()
		bad := []kv{{[]byte("k1"), []byte("b")}, {[]byte("k1"), []byte("a")}}
		if _, err := ks.BulkLoad(seqOf(bad)); !errors.Is(err, ErrBulkLoadOutOfOrder) {
			t.Errorf("= %v, want ErrBulkLoadOutOfOrder", err)
		}
	})
	t.Run("value-size-mismatch", func(t *testing.T) {
		ks, done := newKS(t, 8)
		defer done()
		if _, err := ks.BulkLoad(seqOf([]kv{{[]byte("k"), []byte("short")}})); !errors.Is(err, ErrValueSizeMismatch) {
			t.Errorf("= %v, want ErrValueSizeMismatch", err)
		}
	})
	t.Run("empty-key", func(t *testing.T) {
		ks, done := newKS(t, 0)
		defer done()
		if _, err := ks.BulkLoad(seqOf([]kv{{nil, []byte("v")}})); !errors.Is(err, ErrKeyEmpty) {
			t.Errorf("= %v, want ErrKeyEmpty", err)
		}
	})
	// Indexed SetKeyspace.BulkLoad is covered in bulkload_indexed_test.go;
	// the chunk-8.5 interim "indexed ⇒ error" stub asserted here was
	// deliberately replaced by the real path in 8.6.
}

// TestSetKeyspaceBulkLoadMatchesPut cross-checks a BulkLoad-built SetKeyspace
// against an identical one built via per-member Put: CountValues and
// HasValue must agree for every (key, value).
func TestSetKeyspaceBulkLoadMatchesPut(t *testing.T) {
	ctx := context.Background()
	db := openWith(t, ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 16384})
	defer db.Close()
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()

	data := genSetData(60, func(i int) int {
		if i%5 == 0 {
			return 300 // nested
		}
		return 1 + i%4
	})

	bulkKS, err := tx.CreateSetKeyspace("bulk", nil)
	if err != nil {
		t.Fatalf("CreateSetKeyspace bulk: %v", err)
	}
	if _, err := bulkKS.BulkLoad(setSeq(data)); err != nil {
		t.Fatalf("BulkLoad: %v", err)
	}

	putKS, err := tx.CreateSetKeyspace("put", nil)
	if err != nil {
		t.Fatalf("CreateSetKeyspace put: %v", err)
	}
	for _, d := range data {
		for _, v := range d.values {
			if _, err := putKS.Put(d.key, v); err != nil {
				t.Fatalf("Put(%q,%q): %v", d.key, v, err)
			}
		}
	}

	if bulkKS.desc.Count != putKS.desc.Count {
		t.Fatalf("Count: bulk=%d put=%d", bulkKS.desc.Count, putKS.desc.Count)
	}
	for _, d := range data {
		bc, _ := bulkKS.CountValues(d.key)
		pc, _ := putKS.CountValues(d.key)
		if bc != pc {
			t.Fatalf("CountValues(%q): bulk=%d put=%d", d.key, bc, pc)
		}
		for _, v := range d.values {
			bh, _ := bulkKS.HasValue(d.key, v)
			ph, _ := putKS.HasValue(d.key, v)
			if !bh || !ph {
				t.Fatalf("HasValue(%q,%q): bulk=%v put=%v", d.key, v, bh, ph)
			}
		}
	}
}

// TestSetKeyspaceBulkLoadStorageShapeMatchesPut locks the M-1 fix: a single
// value over the promotion threshold stays a SUBPAGE (as Put's genesis path
// does), and a key only promotes to a nested tree once a second value pushes
// it past the threshold.
func TestSetKeyspaceBulkLoadStorageShapeMatchesPut(t *testing.T) {
	ctx := context.Background()
	db := openWith(t, ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 16384})
	defer db.Close()
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()

	// One ~2050-byte value: a single-value subpage exceeds the ~2040-byte
	// threshold at 4 KB, yet (like Put's genesis) must stay a subpage. Two
	// ~1500-byte values exceed the threshold together → nested.
	big := bytes.Repeat([]byte("Z"), 2050)
	a := bytes.Repeat([]byte("a"), 1500)
	b := bytes.Repeat([]byte("b"), 1500)

	shapeOf := func(ks *SetKeyspace, key []byte) page.LeafEntry {
		t.Helper()
		e, found, err := btree.GetEntry(ks.tx.pgr, ks.builderCfg(), ks.desc.Root, key)
		if err != nil || !found {
			t.Fatalf("GetEntry(%q): found=%v err=%v", key, found, err)
		}
		return e
	}

	bulkKS, err := tx.CreateSetKeyspace("bulk", nil)
	if err != nil {
		t.Fatalf("CreateSetKeyspace bulk: %v", err)
	}
	if _, err := bulkKS.BulkLoad(setSeq([]setData{
		{key: []byte("pair"), values: [][]byte{a, b}},
		{key: []byte("single"), values: [][]byte{big}},
	})); err != nil {
		t.Fatalf("BulkLoad: %v", err)
	}

	if e := shapeOf(bulkKS, []byte("single")); !e.IsSubpage() || e.IsNestedTree() {
		t.Errorf("single large value: IsSubpage=%v IsNestedTree=%v, want subpage", e.IsSubpage(), e.IsNestedTree())
	}
	if e := shapeOf(bulkKS, []byte("pair")); !e.IsNestedTree() {
		t.Errorf("two large values: IsNestedTree=%v, want nested", e.IsNestedTree())
	}

	// Cross-check Put produces the same shape for the single large value.
	putKS, err := tx.CreateSetKeyspace("put", nil)
	if err != nil {
		t.Fatalf("CreateSetKeyspace put: %v", err)
	}
	if _, err := putKS.Put([]byte("single"), big); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if e := shapeOf(putKS, []byte("single")); !e.IsSubpage() {
		t.Errorf("Put single large value: IsSubpage=%v, want subpage (shape parity)", e.IsSubpage())
	}
}

func TestSetKeyspaceBulkLoadReadOnly(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)
	opts := Options{PageSize: 4096, MinSize: 16, MaxSize: 128}
	db := openWith(t, ctx, path, opts)
	defer db.Close()
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if _, err := tx.CreateSetKeyspace("sets", nil); err != nil {
		t.Fatalf("CreateSetKeyspace: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	tx2, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin2: %v", err)
	}
	defer tx2.Rollback()
	ks, err := tx2.OpenSetKeyspaceReadOnly("sets")
	if err != nil {
		t.Fatalf("OpenSetKeyspaceReadOnly: %v", err)
	}
	if _, err := ks.BulkLoad(seqOf([]kv{{[]byte("a"), []byte("b")}})); !errors.Is(err, ErrReadOnly) {
		t.Errorf("= %v, want ErrReadOnly", err)
	}
}

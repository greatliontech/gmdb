package gmdb

import (
	"bytes"
	"context"
	"testing"
)

// TestPutThresholdTieWithOverflowNeighbor pins the legal resident-byte
// tie at the inline threshold T (page-formats.md §Overflow-Key Cells):
// a key of exactly T bytes and an overflow key sharing its first T
// bytes are DISTINCT keys in strict full-key order, storable side by
// side in either insertion order, and each Get returns its own value.
// Regression: the leaf builder's ordering assertion rejected the pair
// (rapid-found; the fail file replays under testdata/rapid/).
func TestPutThresholdTieWithOverflowNeighbor(t *testing.T) {
	ctx := context.Background()
	for _, layout := range []struct {
		name string
		l    LeafLayout
	}{{"segregated", LeafLayoutDefault}, {"interleaved", LeafLayoutInterleaved}} {
		for _, order := range []string{"long-first", "short-first"} {
			t.Run(layout.name+"/"+order, func(t *testing.T) {
				db, err := Open(ctx, t.TempDir()+"/t.db", Options{PageSize: 4096, MinSize: 16, MaxSize: 1 << 16,
					LeafLayout:  layout.l,
					Maintenance: MaintenanceOptions{Disable: true}})
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				tx, err := db.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback()
				ks, err := tx.CreateKeyspace("kv")
				if err != nil {
					t.Fatal(err)
				}
				tSz := int(ks.builderCfg().InlineThreshold())
				short := bytes.Repeat([]byte{1}, tSz)    // exactly T: inline form
				long := bytes.Repeat([]byte{1}, tSz+1)   // T+1: overflow-key form
				longer := bytes.Repeat([]byte{1}, 2*tSz) // 2T: overflow key, same resident
				puts := [][2][]byte{{long, []byte("L")}, {short, []byte("S")}, {longer, []byte("G")}}
				if order == "short-first" {
					puts[0], puts[1] = puts[1], puts[0]
				}
				for _, kv := range puts {
					if err := ks.Put(kv[0], kv[1]); err != nil {
						t.Fatalf("Put(len %d): %v", len(kv[0]), err)
					}
				}
				for _, kv := range puts {
					v, err := ks.Get(kv[0])
					if err != nil || !bytes.Equal(v, kv[1]) {
						t.Fatalf("Get(len %d) = %q, %v; want %q", len(kv[0]), v, err, kv[1])
					}
				}
			})
		}
	}
}

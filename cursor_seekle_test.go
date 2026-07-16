package gmdb

import (
	"bytes"
	"context"
	"fmt"
	"testing"
)

// Cursor.SeekLE / SeekLT (api-surface.md §Cursor API): the backward
// duals of SeekGE — largest key <= / < target — across the full
// boundary matrix, with position coherence (Next/Prev continue from
// the landed entry).

func seekFixture(t *testing.T) *Keyspace {
	t.Helper()
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 256,
		Maintenance: MaintenanceOptions{Disable: true}})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	tx, _ := db.Begin(ctx)
	ks, _ := tx.CreateKeyspace("k")
	for _, k := range []string{"b", "d", "f"} {
		if err := ks.Put([]byte(k), []byte("v"+k)); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	return ks
}

func TestCursorSeekLEBoundaryMatrix(t *testing.T) {
	ks := seekFixture(t)
	c := ks.Cursor()
	defer c.Close()
	cases := []struct {
		target string
		wantLE string // "" = nil
		wantLT string
	}{
		{"a", "", ""},
		{"b", "b", ""},
		{"c", "b", "b"},
		{"d", "d", "b"},
		{"e", "d", "d"},
		{"f", "f", "d"},
		{"z", "f", "f"},
	}
	for _, tc := range cases {
		k, v := c.SeekLE([]byte(tc.target))
		if err := c.Err(); err != nil {
			t.Fatalf("SeekLE(%q): %v", tc.target, err)
		}
		if string(k) != tc.wantLE {
			t.Errorf("SeekLE(%q) = %q, want %q", tc.target, k, tc.wantLE)
		}
		if tc.wantLE != "" && string(v) != "v"+tc.wantLE {
			t.Errorf("SeekLE(%q) value = %q", tc.target, v)
		}
		k, v = c.SeekLT([]byte(tc.target))
		if err := c.Err(); err != nil {
			t.Fatalf("SeekLT(%q): %v", tc.target, err)
		}
		if string(k) != tc.wantLT {
			t.Errorf("SeekLT(%q) = %q, want %q", tc.target, k, tc.wantLT)
		}
		if tc.wantLT != "" && string(v) != "v"+tc.wantLT {
			t.Errorf("SeekLT(%q) value = %q", tc.target, v)
		}
	}

	// Position coherence: iteration continues from the landed entry.
	if k, _ := c.SeekLE([]byte("e")); string(k) != "d" {
		t.Fatalf("SeekLE(e) = %q", k)
	}
	if k, _ := c.Next(); string(k) != "f" {
		t.Errorf("Next after SeekLE(e) = %q, want f", k)
	}
	if k, _ := c.SeekLT([]byte("f")); string(k) != "d" {
		t.Fatalf("SeekLT(f) = %q", k)
	}
	if k, _ := c.Prev(); string(k) != "b" {
		t.Errorf("Prev after SeekLT(f) = %q, want b", k)
	}
}

func TestCursorSeekLEEmptyKeyspace(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 256,
		Maintenance: MaintenanceOptions{Disable: true}})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	tx, _ := db.Begin(ctx)
	ks, _ := tx.CreateKeyspace("k")
	c := ks.Cursor()
	defer c.Close()
	if k, _ := c.SeekLE([]byte("x")); k != nil {
		t.Errorf("SeekLE on empty = %q, want nil", k)
	}
	if k, _ := c.SeekLT([]byte("x")); k != nil {
		t.Errorf("SeekLT on empty = %q, want nil", k)
	}
	if err := c.Err(); err != nil {
		t.Fatalf("Err: %v", err)
	}
}

// TestCursorSeekLEAcrossLeaves: the floor's predecessor step crosses a
// leaf boundary (the successor is the first entry of a leaf; the
// floor is the previous leaf's last) — exercised by a multi-leaf tree
// probing between the leaves' boundary keys.
func TestCursorSeekLEAcrossLeaves(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 1 << 12,
		Maintenance: MaintenanceOptions{Disable: true}})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	tx, _ := db.Begin(ctx)
	ks, _ := tx.CreateKeyspace("k")
	const n = 500
	for i := range n {
		// Even keys only, so every odd probe is a miss with a
		// same-tree successor and predecessor.
		if err := ks.Put(fmt.Appendf(nil, "key%06d", i*2), bytes.Repeat([]byte{'v'}, 40)); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	c := ks.Cursor()
	defer c.Close()
	for i := 1; i < n; i += 37 {
		probe := fmt.Appendf(nil, "key%06d", i*2-1) // odd — always a miss
		want := fmt.Appendf(nil, "key%06d", (i-1)*2)
		k, _ := c.SeekLE(probe)
		if err := c.Err(); err != nil {
			t.Fatalf("SeekLE(%s): %v", probe, err)
		}
		if !bytes.Equal(k, want) {
			t.Fatalf("SeekLE(%s) = %s, want %s", probe, k, want)
		}
		// SeekLT of an EXACT key steps to the true predecessor.
		exact := fmt.Appendf(nil, "key%06d", i*2)
		k, _ = c.SeekLT(exact)
		if !bytes.Equal(k, want) {
			t.Fatalf("SeekLT(%s) = %s, want %s", exact, k, want)
		}
	}
}

// TestSetCursorSeekLE: the key-level floor duals on SetCursor land on
// (floorKey, firstValueOfFloorKey).
func TestSetCursorSeekLE(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 256,
		Maintenance: MaintenanceOptions{Disable: true}})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	tx, _ := db.Begin(ctx)
	sks, _ := tx.CreateSetKeyspace("s", nil)
	for _, kv := range [][2]string{{"b", "1"}, {"b", "2"}, {"d", "9"}} {
		if _, err := sks.Put([]byte(kv[0]), []byte(kv[1])); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	c := sks.Cursor()
	if k, v := c.SeekLE([]byte("c")); string(k) != "b" || string(v) != "1" {
		t.Errorf("SeekLE(c) = (%q,%q), want (b,1)", k, v)
	}
	if k, v := c.SeekLT([]byte("d")); string(k) != "b" || string(v) != "1" {
		t.Errorf("SeekLT(d) = (%q,%q), want (b,1)", k, v)
	}
	if k, _ := c.SeekLT([]byte("b")); k != nil {
		t.Errorf("SeekLT(b) = %q, want nil", k)
	}
	if k, v := c.SeekLE([]byte("z")); string(k) != "d" || string(v) != "9" {
		t.Errorf("SeekLE(z) = (%q,%q), want (d,9)", k, v)
	}
	if err := c.Err(); err != nil {
		t.Fatalf("Err: %v", err)
	}
}

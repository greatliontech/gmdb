package page

import (
	"bytes"
	"errors"
	"testing"
)

// Segregated-branch white-box coverage beyond the shared battery in
// branch_plain_test.go (which runs both layouts): forged-page
// rejections at segregated offsets, and the marker-preserving child
// rewrite.

func segBranchCfg() Config {
	return Config{PageSize: 4096, BranchLayout: BranchLayoutSegregated}
}

// TestSegBranchValidateRejectsForged pins segValidateBranch's checks
// with in-bounds forgeries that only the targeted rule can reject.
func TestSegBranchValidateRejectsForged(t *testing.T) {
	cfg := segBranchCfg()
	tt := cfg.InlineThreshold()

	build := func(cells []BranchCell, mutate func(buf []byte)) error {
		t.Helper()
		buf := make([]byte, cfg.PageSize)
		if err := EncodeBranch(buf, cfg, 1, cells); err != nil {
			t.Fatalf("EncodeBranch: %v", err)
		}
		if mutate != nil {
			mutate(buf)
		}
		return ValidateBranch(buf, cfg)
	}
	plainCells := []BranchCell{
		{Key: []byte("shared-aaa"), Child: 1},
		{Key: []byte("shared-bbb"), Child: 2},
		{Key: []byte("shared-ccc"), Child: 3},
	}
	if err := build(plainCells, nil); err != nil {
		t.Fatalf("clean page rejected: %v", err)
	}

	t.Run("directory offset monotonicity", func(t *testing.T) {
		err := build(plainCells, func(buf []byte) {
			// Swap slots 1 and 2 (heap-relative offsets 3 and 6).
			s1 := le.Uint16(buf[segBranchDirOff+1*segBranchDirSlotSize:])
			s2 := le.Uint16(buf[segBranchDirOff+2*segBranchDirSlotSize:])
			le.PutUint16(buf[segBranchDirOff+1*segBranchDirSlotSize:], s2)
			le.PutUint16(buf[segBranchDirOff+2*segBranchDirSlotSize:], s1)
		})
		if !errors.Is(err, ErrCorrupted) {
			t.Errorf("got %v, want ErrCorrupted", err)
		}
	})
	t.Run("heap overruns child array", func(t *testing.T) {
		err := build(plainCells, func(buf []byte) {
			// Forge the sentinel far past the child-array base.
			le.PutUint16(buf[segBranchDirOff+3*segBranchDirSlotSize:], 0x7000)
		})
		if !errors.Is(err, ErrCorrupted) {
			t.Errorf("got %v, want ErrCorrupted", err)
		}
	})
	t.Run("prefix length exceeds threshold", func(t *testing.T) {
		err := build(plainCells, func(buf []byte) {
			le.PutUint16(buf[segBranchPrefixLenOff:], uint16(tt+1))
		})
		if !errors.Is(err, ErrCorrupted) {
			t.Errorf("got %v, want ErrCorrupted", err)
		}
	})
	t.Run("full key exceeds threshold", func(t *testing.T) {
		// Widen cell 2's span so PrefixLen+span > T while every offset
		// stays in bounds: shrink slot 2 to zero (cell 1 absorbs the
		// bytes) is monotone-legal; instead grow the sentinel modestly
		// past a T-boundary. Build a page whose last cell can absorb
		// the growth within the heap window.
		long := bytes.Repeat([]byte{'k'}, tt-2)
		cells := []BranchCell{
			{Key: []byte("aa"), Child: 1},
			{Key: long, Child: 2},
		}
		err := build(cells, func(buf []byte) {
			// Sentinel += 3 pushes cell 1's span to tt+1 > T. The heap
			// window has slack (free space precedes the child array).
			s := le.Uint16(buf[segBranchDirOff+2*segBranchDirSlotSize:])
			le.PutUint16(buf[segBranchDirOff+2*segBranchDirSlotSize:], s+3)
		})
		if !errors.Is(err, ErrCorrupted) {
			t.Errorf("got %v, want ErrCorrupted", err)
		}
	})

	// Overflow-cell forgeries need an ovk fixture.
	sepA := bytes.Repeat([]byte{'a'}, tt+9)
	ovkCells := []BranchCell{
		{Key: sepA[:tt], Child: 7, KeyExtPage: 3, KeyTotalLen: uint32(len(sepA))},
		{Key: append(bytes.Repeat([]byte{'a'}, 4), 'z'), Child: 8},
	}
	if err := build(ovkCells, nil); err != nil {
		t.Fatalf("clean ovk page rejected: %v", err)
	}
	hb := func(n int) int { return segBranchHeapBase(n) }
	t.Run("ovk span mismatch", func(t *testing.T) {
		err := build(ovkCells, func(buf []byte) {
			// Shrink the ovk cell's span by bumping slot 0's start? Slot 0
			// is 0; instead shrink slot 1 by one — the span check fires.
			s := le.Uint16(buf[segBranchDirOff+1*segBranchDirSlotSize:])
			le.PutUint16(buf[segBranchDirOff+1*segBranchDirSlotSize:], s-1)
		})
		if !errors.Is(err, ErrCorrupted) {
			t.Errorf("got %v, want ErrCorrupted", err)
		}
	})
	t.Run("ovk zero extent page", func(t *testing.T) {
		err := build(ovkCells, func(buf []byte) {
			m := segBranchPrefixLen(buf)
			inlineEnd := hb(2) + segBranchDirSlot(buf, 1) - branchKeyExtRefSize
			_ = m
			le.PutUint64(buf[inlineEnd:], 0)
		})
		if !errors.Is(err, ErrCorrupted) {
			t.Errorf("got %v, want ErrCorrupted", err)
		}
	})
	t.Run("ovk KeyTotalLen under threshold", func(t *testing.T) {
		err := build(ovkCells, func(buf []byte) {
			inlineEnd := hb(2) + segBranchDirSlot(buf, 1) - branchKeyExtRefSize
			le.PutUint32(buf[inlineEnd+8:], uint32(tt))
		})
		if !errors.Is(err, ErrCorrupted) {
			t.Errorf("got %v, want ErrCorrupted", err)
		}
	})
}

// TestSegBranchChildRewritePreservesOverflowMarker: SetBranchCellChild
// on a segregated overflow cell must keep the bit-63 marker (the
// marker rides the child word — page-formats.md §Segregated Branch).
func TestSegBranchChildRewritePreservesOverflowMarker(t *testing.T) {
	cfg := segBranchCfg()
	tt := cfg.InlineThreshold()
	sepA := bytes.Repeat([]byte{'a'}, tt+9)
	cells := []BranchCell{
		{Key: sepA[:tt], Child: 7, KeyExtPage: 3, KeyTotalLen: uint32(len(sepA))},
		{Key: append(bytes.Repeat([]byte{'a'}, 4), 'z'), Child: 8},
	}
	buf := make([]byte, cfg.PageSize)
	if err := EncodeBranch(buf, cfg, 1, cells); err != nil {
		t.Fatalf("EncodeBranch: %v", err)
	}
	SetBranchCellChild(buf, cfg, 0, 700)
	got := BranchCellAt(buf, cfg, 0)
	if got.Child != 700 || got.KeyExtPage != 3 || got.KeyTotalLen != uint32(len(sepA)) {
		t.Fatalf("after child rewrite: %+v (marker or extent fields lost)", got)
	}
	if err := ValidateBranch(buf, cfg); err != nil {
		t.Fatalf("Validate after child rewrite: %v", err)
	}
	SetBranchCellKeyExtPage(buf, cfg, 0, 300)
	got = BranchCellAt(buf, cfg, 0)
	if got.KeyExtPage != 300 || got.Child != 700 {
		t.Fatalf("after keyext rewrite: %+v", got)
	}
}

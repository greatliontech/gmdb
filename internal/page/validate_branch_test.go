package page

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// TestValidateBranchAcceptsWellFormed: a branch page built by
// EncodeBranch passes ValidateBranch.
func TestValidateBranchAcceptsWellFormed(t *testing.T) {
	cfg := Config{PageSize: 4096}
	buf := make([]byte, cfg.PageSize)
	cells := []BranchCell{
		{Key: []byte("ccc"), Child: 10},
		{Key: []byte("mmm"), Child: 20},
		{Key: []byte("ttt"), Child: 30},
	}
	if err := EncodeBranch(buf, cfg, 5, cells); err != nil {
		t.Fatalf("EncodeBranch: %v", err)
	}
	if err := ValidateBranch(buf, cfg); err != nil {
		t.Errorf("ValidateBranch(well-formed) = %v, want nil", err)
	}
	// And the empty branch.
	empty := make([]byte, cfg.PageSize)
	EncodeBranchEmpty(empty, cfg, 7)
	if err := ValidateBranch(empty, cfg); err != nil {
		t.Errorf("ValidateBranch(empty) = %v, want nil", err)
	}
}

// TestValidateBranchRejectsForged (Inv-C1): every forged shape returns a
// wrapped ErrCorrupted and never panics.
func TestValidateBranchRejectsForged(t *testing.T) {
	cfg := Config{PageSize: 4096}
	base := func() []byte {
		b := make([]byte, cfg.PageSize)
		_ = EncodeBranch(b, cfg, 5, []BranchCell{{Key: []byte("mmm"), Child: 20}})
		return b
	}

	t.Run("wrong type", func(t *testing.T) {
		b := make([]byte, cfg.PageSize)
		WriteHeader(b, TypeLeaf, 1, 0)
		if err := ValidateBranch(b, cfg); !errors.Is(err, ErrCorrupted) {
			t.Errorf("got %v, want ErrCorrupted", err)
		}
	})

	t.Run("directory overruns content end", func(t *testing.T) {
		b := base()
		// Forge a huge cell count: the directory cannot fit.
		WriteHeader(b, TypeBranch, 0xFFFF, 0)
		if err := ValidateBranch(b, cfg); !errors.Is(err, ErrCorrupted) {
			t.Errorf("got %v, want ErrCorrupted", err)
		}
	})

	t.Run("cell offset out of range", func(t *testing.T) {
		b := base()
		// First directory entry begins at branchHeaderEnd (offset 16):
		// (Offset uint16, KeyLen uint16). Forge the offset past ContentEnd.
		b[branchHeaderEnd] = 0xFF   // dir[0] offset low byte
		b[branchHeaderEnd+1] = 0xFF // high byte → offset 0xFFFF
		if err := ValidateBranch(b, cfg); !errors.Is(err, ErrCorrupted) {
			t.Errorf("got %v, want ErrCorrupted", err)
		}
	})

	t.Run("inline key length exceeds threshold", func(t *testing.T) {
		// A plain cell can never store an over-T key (page-formats.md
		// §Overflow-Key Cells — over-T keys must take the overflow
		// form). The forgery must stay INSIDE the page bounds so only
		// this rule can reject: cell 1 packs below cell 0's T-byte
		// key, so KeyLen = T+1 reads into cell 0's bytes without
		// overrunning ContentEnd.
		tt := cfg.InlineThreshold()
		b := make([]byte, cfg.PageSize)
		cells := []BranchCell{
			{Key: bytes.Repeat([]byte{'a'}, tt), Child: 1},
			{Key: []byte("bbb"), Child: 2},
		}
		if err := EncodeBranch(b, cfg, 5, cells); err != nil {
			t.Fatalf("EncodeBranch: %v", err)
		}
		le.PutUint16(b[branchHeaderEnd+branchDirEntrySize+2:], uint16(tt+1))
		err := ValidateBranch(b, cfg)
		if !errors.Is(err, ErrCorrupted) {
			t.Fatalf("got %v, want ErrCorrupted", err)
		}
		if !strings.Contains(err.Error(), "exceeds inline threshold") {
			t.Errorf("rejection reached the wrong rule: %v", err)
		}
	})

	t.Run("buffer too small", func(t *testing.T) {
		b := make([]byte, 16) // smaller than content end
		WriteHeader(b, TypeBranch, 0, 0)
		if err := ValidateBranch(b, cfg); !errors.Is(err, ErrCorrupted) {
			t.Errorf("got %v, want ErrCorrupted", err)
		}
	})
}

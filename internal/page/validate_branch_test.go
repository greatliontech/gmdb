package page

import (
	"errors"
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
		b[16] = 0xFF // first dir entry offset low byte
		b[17] = 0xFF // high byte → offset 0xFFFF > ContentEnd
		if err := ValidateBranch(b, cfg); !errors.Is(err, ErrCorrupted) {
			t.Errorf("got %v, want ErrCorrupted", err)
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

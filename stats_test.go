package gmdb

import (
	"context"
	"errors"
	"testing"
)

// A stats walk over a corrupt tree surfaces the PUBLIC error
// surface (here ErrBadPageChecksum — the checksum-corruption
// class), never a raw internal walk error: the mapping is the
// walkTreePageStats wrapper's single job, shared by every stats
// consumer (KeyspaceStats, IndexStats).
func TestStatsCorruptTreeSurfacesPublicSentinel(t *testing.T) {
	ctx := context.Background()
	path := corruptRootChecksumDB(t, ctx)

	db, err := Open(ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 256})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db.Close()

	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()
	ks, err := tx.OpenKeyspaceReadOnly("k")
	if err != nil {
		t.Fatalf("OpenKeyspaceReadOnly: %v", err)
	}

	_, err = ks.Stats()
	if err == nil {
		t.Fatal("Stats over a corrupt tree = nil error, want ErrBadPageChecksum")
	}
	if !errors.Is(err, ErrBadPageChecksum) {
		t.Fatalf("Stats over a corrupt tree = %v, want errors.Is(_, ErrBadPageChecksum)", err)
	}
}

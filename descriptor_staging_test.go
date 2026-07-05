package gmdb

import (
	"context"
	"errors"
	"testing"
)

// Same-tx descriptor staging: a descriptor mutation staged in
// tx.dirtyDescriptors while its keyspace is uncached (TxIndexes.Drop /
// Rebuild, SetKeyspaceConfig) must survive a subsequent open of that
// keyspace in the same transaction. The open transfers the staged
// entry's flush obligation into the cached handle (openCacheState);
// discarding it instead leaves the on-disk descriptor untouched at
// Commit while the mutation's page-level effects (FreeSubtree of the
// dropped index trees) land — the registry entry resurrects pointing
// at freed pages.

// stagingFixture holds a committed DB with an indexed Kind=0 keyspace
// "t" (indexes keep + victim) and an indexed Kind=1 set keyspace "s"
// (indexes skeep + svictim), each with a row so every index data tree
// owns at least one page for Drop to free.
type stagingFixture struct {
	db             *DB
	keep, victim   *IndexDecl
	skeep, svictim *IndexDecl
}

func newStagingFixture(t *testing.T) *stagingFixture {
	t.Helper()
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 4096})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	fx := &stagingFixture{
		db: db,
		keep: &IndexDecl{
			Name:    "keep",
			Columns: []IndexColumn{{Name: "c"}},
			Extract: func(key, value []byte) []IndexEntry {
				return []IndexEntry{{Cols: [][]byte{value[:1]}}}
			},
		},
		victim: &IndexDecl{
			Name:    "victim",
			Columns: []IndexColumn{{Name: "k"}},
			Extract: func(key, value []byte) []IndexEntry {
				return []IndexEntry{{Cols: [][]byte{key}}}
			},
		},
		skeep: &IndexDecl{
			Name:    "skeep",
			Columns: []IndexColumn{{Name: "m"}},
			Extract: func(setKey, member []byte) []IndexEntry {
				return []IndexEntry{{Cols: [][]byte{member[:1]}}}
			},
		},
		svictim: &IndexDecl{
			Name:    "svictim",
			Columns: []IndexColumn{{Name: "m2"}},
			Extract: func(setKey, member []byte) []IndexEntry {
				return []IndexEntry{{Cols: [][]byte{member}}}
			},
		},
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ks, err := tx.CreateKeyspace("t", fx.keep, fx.victim)
	if err != nil {
		t.Fatal(err)
	}
	if err := ks.Put([]byte("k1"), []byte("abc")); err != nil {
		t.Fatal(err)
	}
	sks, err := tx.CreateSetKeyspace("s", nil, fx.skeep, fx.svictim)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sks.Put([]byte("set"), []byte("member")); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return fx
}

// verifyVictimDropped asserts the post-commit state a persisted Drop
// must leave: supplying the dropped decl fails ErrIndexUnknown, the
// surviving open serves no handle for the dropped name, and Check
// reports a fully consistent database (in particular no registry entry
// pointing at freed index pages).
func (fx *stagingFixture) verifyVictimDropped(t *testing.T, set bool) {
	t.Helper()
	ctx := context.Background()
	tx, err := fx.db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if set {
		if _, err := tx.OpenSetKeyspace("s", fx.skeep, fx.svictim); !errors.Is(err, ErrIndexUnknown) {
			t.Errorf("OpenSetKeyspace with dropped decl: err = %v, want ErrIndexUnknown", err)
		}
		sks, err := tx.OpenSetKeyspace("s", fx.skeep)
		if err != nil {
			t.Fatalf("OpenSetKeyspace without dropped decl: %v", err)
		}
		if _, err := sks.Index("svictim"); !errors.Is(err, ErrIndexNotFound) {
			t.Errorf("Index(svictim) after persisted drop: err = %v, want ErrIndexNotFound", err)
		}
	} else {
		if _, err := tx.OpenKeyspace("t", fx.keep, fx.victim); !errors.Is(err, ErrIndexUnknown) {
			t.Errorf("OpenKeyspace with dropped decl: err = %v, want ErrIndexUnknown", err)
		}
		ks, err := tx.OpenKeyspace("t", fx.keep)
		if err != nil {
			t.Fatalf("OpenKeyspace without dropped decl: %v", err)
		}
		if _, err := ks.Index("victim"); !errors.Is(err, ErrIndexNotFound) {
			t.Errorf("Index(victim) after persisted drop: err = %v, want ErrIndexNotFound", err)
		}
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	for iss := range fx.db.Check() {
		t.Errorf("Check after persisted drop: [%s] %s (page %d, keyspace %q, index %q)",
			iss.Code, iss.Message, iss.PageID, iss.Keyspace, iss.Index)
	}
}

// TestDropThenOpenSameTxPersists pins staged-descriptor conservation
// across every open path that can follow a TxIndexes.Drop on an
// uncached keyspace in the same transaction. Without the flush-state
// transfer, each variant committed a resurrected registry entry whose
// root pointed at FreeSubtree'd pages.
func TestDropThenOpenSameTxPersists(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name string
		set  bool
		open func(t *testing.T, fx *stagingFixture, tx *Tx)
	}{
		{"OpenKeyspace", false, func(t *testing.T, fx *stagingFixture, tx *Tx) {
			ks, err := tx.OpenKeyspace("t", fx.keep)
			if err != nil {
				t.Fatal(err)
			}
			// Mutate through the opened handle so the flushed
			// descriptor must be the handle's post-open state, not
			// the pre-open staged snapshot.
			if err := ks.Put([]byte("k2"), []byte("xyz")); err != nil {
				t.Fatal(err)
			}
		}},
		{"OpenKeyspaceReadOnly", false, func(t *testing.T, fx *stagingFixture, tx *Tx) {
			if _, err := tx.OpenKeyspaceReadOnly("t"); err != nil {
				t.Fatal(err)
			}
		}},
		{"CreateKeyspaceIfNotExists", false, func(t *testing.T, fx *stagingFixture, tx *Tx) {
			if _, err := tx.CreateKeyspaceIfNotExists("t", fx.keep); err != nil {
				t.Fatal(err)
			}
		}},
		{"OpenSetKeyspace", true, func(t *testing.T, fx *stagingFixture, tx *Tx) {
			sks, err := tx.OpenSetKeyspace("s", fx.skeep)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := sks.Put([]byte("set"), []byte("m2")); err != nil {
				t.Fatal(err)
			}
		}},
		{"OpenSetKeyspaceReadOnly", true, func(t *testing.T, fx *stagingFixture, tx *Tx) {
			if _, err := tx.OpenSetKeyspaceReadOnly("s"); err != nil {
				t.Fatal(err)
			}
		}},
		{"CreateSetKeyspaceIfNotExists", true, func(t *testing.T, fx *stagingFixture, tx *Tx) {
			if _, err := tx.CreateSetKeyspaceIfNotExists("s", nil, fx.skeep); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := newStagingFixture(t)
			tx, err := fx.db.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			ksName, idxName := "t", "victim"
			if tc.set {
				ksName, idxName = "s", "svictim"
			}
			if err := tx.Indexes().Drop(ksName, idxName); err != nil {
				t.Fatalf("Drop on uncached keyspace: %v", err)
			}
			tc.open(t, fx, tx)
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			fx.verifyVictimDropped(t, tc.set)
		})
	}
}

// TestRebuildThenOpenSameTxPersists: the Rebuild sibling of the Drop
// case — TxIndexes.Rebuild on an uncached keyspace stages the updated
// descriptor through the same adapter path; a same-tx open must not
// discard the staged registry state (here: the rebuilt index's new
// UserVersion).
func TestRebuildThenOpenSameTxPersists(t *testing.T) {
	ctx := context.Background()
	fx := newStagingFixture(t)
	victimV2 := &IndexDecl{
		Name:    fx.victim.Name,
		Columns: fx.victim.Columns,
		Version: "2",
		Extract: fx.victim.Extract,
	}
	tx, err := fx.db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := tx.Indexes().Rebuild("t", victimV2); err != nil {
		t.Fatalf("Rebuild on uncached keyspace: %v", err)
	}
	if _, err := tx.OpenKeyspace("t", fx.keep, victimV2); err != nil {
		t.Fatalf("same-tx open after Rebuild: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	check, err := fx.db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer check.Rollback()
	var fpErr *IndexFingerprintError
	if _, err := check.OpenKeyspace("t", fx.keep, fx.victim); !errors.As(err, &fpErr) {
		t.Errorf("open with pre-rebuild Version: err = %v, want IndexFingerprintError (staged rebuild lost by same-tx open)", err)
	}
	if _, err := check.OpenKeyspace("t", fx.keep, victimV2); err != nil {
		t.Errorf("open with rebuilt Version: %v", err)
	}
	if err := check.Rollback(); err != nil {
		t.Fatal(err)
	}
	for iss := range fx.db.Check() {
		t.Errorf("Check after persisted rebuild: [%s] %s (page %d, keyspace %q, index %q)",
			iss.Code, iss.Message, iss.PageID, iss.Keyspace, iss.Index)
	}
}

// TestSetKeyspaceConfigThenOpenSameTxPersists: the config sibling of
// the Drop case — SetKeyspaceConfig on an uncached keyspace stages the
// descriptor; a same-tx open must not discard the staged
// RestartGroupTarget.
func TestSetKeyspaceConfigThenOpenSameTxPersists(t *testing.T) {
	ctx := context.Background()
	for _, ro := range []bool{false, true} {
		name := "Open"
		if ro {
			name = "OpenReadOnly"
		}
		t.Run(name, func(t *testing.T) {
			db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 4096})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { db.Close() })
			tx, err := db.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := tx.CreateKeyspace("cfg"); err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}

			tx, err = db.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			if err := tx.SetKeyspaceConfig("cfg", KeyspaceConfig{RestartGroupTarget: 7}); err != nil {
				t.Fatal(err)
			}
			if ro {
				if _, err := tx.OpenKeyspaceReadOnly("cfg"); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := tx.OpenKeyspace("cfg"); err != nil {
					t.Fatal(err)
				}
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}

			check, err := db.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer check.Rollback()
			desc, found, err := check.loadDescriptor("cfg")
			if err != nil || !found {
				t.Fatalf("loadDescriptor: found=%v err=%v", found, err)
			}
			if desc.RestartGroupTarget != 7 {
				t.Errorf("persisted RestartGroupTarget = %d, want 7 (staged config lost by same-tx open)",
					desc.RestartGroupTarget)
			}
		})
	}
}

// TestDropThenChildOpenPersists: nested variants of the conservation
// rule. Parent-staged: the child clone carries the staged entry, the
// child's open transfers it into a child handle, and the merge installs
// that handle (state included) into the parent. Child-staged: the child
// both stages and opens; the merged handle must still flush at the
// top-level Commit.
func TestDropThenChildOpenPersists(t *testing.T) {
	ctx := context.Background()
	for _, childStages := range []bool{false, true} {
		name := "ParentStagesChildOpens"
		if childStages {
			name = "ChildStagesChildOpens"
		}
		t.Run(name, func(t *testing.T) {
			fx := newStagingFixture(t)
			tx, err := fx.db.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			if !childStages {
				if err := tx.Indexes().Drop("t", "victim"); err != nil {
					t.Fatal(err)
				}
			}
			child, err := tx.BeginChild()
			if err != nil {
				t.Fatal(err)
			}
			if childStages {
				if err := child.Indexes().Drop("t", "victim"); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := child.OpenKeyspace("t", fx.keep); err != nil {
				t.Fatal(err)
			}
			if err := child.Commit(); err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			fx.verifyVictimDropped(t, false)
		})
	}
}

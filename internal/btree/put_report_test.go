package btree

import (
	"bytes"
	"testing"

	"github.com/thegrumpylion/gmdb/internal/page"
)

// PutReportExisting and InsertIfAbsent are the single-descent variants
// that collapse the Has-then-Put double descent (set-keyspace.md
// putIntoNestedTree, keyspace.go non-indexed Put). PutReportExisting
// always writes and reports replace-vs-insert; InsertIfAbsent skips the
// write entirely when the key is present (no CoW, no alloc) so a
// duplicate set-insert neither churns nor orphans pages.

func TestPutReportExistingReportsInsertVsReplace(t *testing.T) {
	cfg := page.Config{PageSize: 4096}
	pw := newFakeWriter(t, 4096)

	// Empty tree: a genesis insert is not a replace.
	root, existed, err := PutReportExisting(pw, cfg, 0, []byte("k"), []byte("v1"))
	if err != nil {
		t.Fatalf("PutReportExisting (empty): %v", err)
	}
	if existed {
		t.Errorf("genesis insert: existed=true, want false")
	}

	// New key in a non-empty tree: insert, not replace.
	root, existed, err = PutReportExisting(pw, cfg, root, []byte("k2"), []byte("v2"))
	if err != nil {
		t.Fatalf("PutReportExisting (new key): %v", err)
	}
	if existed {
		t.Errorf("new-key insert: existed=true, want false")
	}

	// Existing key: replace, and the value is updated.
	root, existed, err = PutReportExisting(pw, cfg, root, []byte("k"), []byte("v1b"))
	if err != nil {
		t.Fatalf("PutReportExisting (replace): %v", err)
	}
	if !existed {
		t.Errorf("replace: existed=false, want true")
	}
	got, found, err := Get(pw, cfg, root, []byte("k"))
	if err != nil || !found {
		t.Fatalf("Get after replace: found=%v err=%v", found, err)
	}
	if !bytes.Equal(got, []byte("v1b")) {
		t.Errorf("replaced value = %q, want v1b", got)
	}
}

func TestInsertIfAbsentNoOpOnPresent(t *testing.T) {
	cfg := page.Config{PageSize: 4096}
	pw := newFakeWriter(t, 4096)

	// Insert into empty tree: added.
	root, added, err := InsertIfAbsent(pw, cfg, 0, []byte("k"), nil)
	if err != nil {
		t.Fatalf("InsertIfAbsent (empty): %v", err)
	}
	if !added {
		t.Errorf("genesis insert: added=false, want true")
	}

	// Duplicate insert: NOT added, and a true no-op — the root is
	// unchanged (no CoW) and no page is allocated. allocBefore captures
	// the fakeWriter's monotonic page counter; a CoW would bump it and
	// return a fresh root, orphaning the rewritten pages on commit
	// (the leak the issue's single-PutReportExisting sketch would cause).
	allocBefore := pw.nextID
	root2, added, err := InsertIfAbsent(pw, cfg, root, []byte("k"), nil)
	if err != nil {
		t.Fatalf("InsertIfAbsent (duplicate): %v", err)
	}
	if added {
		t.Errorf("duplicate insert: added=true, want false")
	}
	if root2 != root {
		t.Errorf("duplicate insert changed root %d -> %d; must be a no-op (no CoW)", root, root2)
	}
	if pw.nextID != allocBefore {
		t.Errorf("duplicate insert allocated %d page(s); InsertIfAbsent must not allocate on present", pw.nextID-allocBefore)
	}

	// A different key is still inserted (added, root changes).
	root3, added, err := InsertIfAbsent(pw, cfg, root, []byte("k2"), nil)
	if err != nil {
		t.Fatalf("InsertIfAbsent (new key): %v", err)
	}
	if !added {
		t.Errorf("new-key insert: added=false, want true")
	}
	if root3 == root {
		t.Errorf("new-key insert did not change root")
	}
}

// TestInsertIfAbsentNoOpOnPresentMultiLevel exercises the no-op-on-present
// contract when the present key lives below a branch level, so the
// existence peek happens after a multi-page descent (the path the
// redundant Has used to re-walk).
func TestInsertIfAbsentNoOpOnPresentMultiLevel(t *testing.T) {
	cfg := page.Config{PageSize: 4096}
	pw := newFakeWriter(t, 4096)

	// Insert enough fixed-size members to force at least one leaf split
	// (a branch root over multiple leaves).
	root := uint64(0)
	val := bytes.Repeat([]byte{'x'}, 200)
	for i := range 64 {
		k := []byte{byte(i)}
		nr, added, err := InsertIfAbsent(pw, cfg, root, k, val)
		if err != nil {
			t.Fatalf("InsertIfAbsent(%d): %v", i, err)
		}
		if !added {
			t.Fatalf("InsertIfAbsent(%d): added=false on fresh key", i)
		}
		root = nr
	}
	// Re-insert an existing key that requires descending through a branch.
	allocBefore := pw.nextID
	nr, added, err := InsertIfAbsent(pw, cfg, root, []byte{32}, val)
	if err != nil {
		t.Fatalf("InsertIfAbsent (present, multi-level): %v", err)
	}
	if added {
		t.Errorf("present multi-level: added=true, want false")
	}
	if nr != root {
		t.Errorf("present multi-level changed root %d -> %d", root, nr)
	}
	if pw.nextID != allocBefore {
		t.Errorf("present multi-level allocated %d page(s); want 0", pw.nextID-allocBefore)
	}
}

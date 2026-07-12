package gmdb

import (
	"bytes"
	"context"
	"testing"

	"github.com/greatliontech/gmdb/internal/indexing"
)

// The registry-entry flush REBUILDS entries from pinned state; a
// stored per-kind payload must round-trip through it (indexing.md
// §Storage Layout). Unreachable end-to-end today — the open,
// rebuild, and drop gates reject non-composite kinds, and the
// codec rejects a composite entry carrying a payload — so this
// drives the flush half white-box: a pinned index whose in-memory
// kind/payload mimic a future non-composite kind must survive
// flushIndexRegistry -> registryGet byte-identical.
func TestIndexRegistryFlushRoundTripsKindPayload(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()
	decl := &IndexDecl{
		Name:    "ix",
		Columns: []IndexColumn{{Name: "a"}},
		Extract: func(k, v []byte) []IndexEntry {
			return []IndexEntry{{Cols: [][]byte{v}}}
		},
	}
	ks, err := tx.CreateKeyspace("k", decl)
	if err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	if err := ks.Put([]byte("k1"), []byte("v1")); err != nil {
		t.Fatalf("Put: %v", err)
	}

	// Mimic a future non-composite kind in pinned state (in-memory
	// only; no gate re-checks within the owning tx).
	payload := []byte{0xDE, 0xAD, 0xBE, 0xEF, 0x00, 0x01}
	p := ks.indexes["ix"]
	p.decl.Kind = indexing.Kind(7)
	p.kindPayload = payload

	if err := tx.flushIndexRegistry(&ks.keyspaceCore, ks.indexes); err != nil {
		t.Fatalf("flushIndexRegistry: %v", err)
	}
	entry, err := tx.registryGet(&ks.keyspaceCore, "ix")
	if err != nil {
		t.Fatalf("registryGet: %v", err)
	}
	if entry.Kind != indexing.Kind(7) || !bytes.Equal(entry.KindPayload, payload) {
		t.Fatalf("flushed entry kind=%d payload=%x, want kind=7 payload=%x — the flush dropped the per-kind payload", entry.Kind, entry.KindPayload, payload)
	}

	// The rebuild seam: a rebuild writes a fresh, payload-less
	// entry AND must clear the pinned copy — otherwise the next
	// flush resurrects the old payload over the fresh entry.
	p.decl.Kind = indexing.KindComposite // the rebuild gates admit composite only
	tx.syncRebuildToCachedPinned(ks, nil, p.decl, p.root, p.count)
	if p.kindPayload != nil {
		t.Fatalf("pinned payload survived the rebuild sync: %x — flush would resurrect it", p.kindPayload)
	}
	if err := tx.flushIndexRegistry(&ks.keyspaceCore, ks.indexes); err != nil {
		t.Fatalf("flushIndexRegistry after sync: %v", err)
	}
	entry, err = tx.registryGet(&ks.keyspaceCore, "ix")
	if err != nil {
		t.Fatalf("registryGet: %v", err)
	}
	if len(entry.KindPayload) != 0 {
		t.Fatalf("post-rebuild flushed payload = %x, want empty (stale payload resurrected)", entry.KindPayload)
	}
}

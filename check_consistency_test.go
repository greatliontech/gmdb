package gmdb

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/thegrumpylion/gmdb/internal/page"
)

// buildConsistencyFixture creates a checksums-off database (so page
// footers cannot mask byte surgery) with a plain keyspace spanning
// multiple leaves and a set keyspace holding one promoted nested
// tree, then closes it and returns the path.
func buildConsistencyFixture(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	path := tmpPath(t)
	db, err := Open(ctx, path, Options{
		PageSize: 4096, MinSize: 16, MaxSize: 256,
		PageChecksum: false, // explicit: surgery must not be masked by footers
		Maintenance:  MaintenanceOptions{Disable: true},
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := db.Update(ctx, func(tx *Tx) error {
		ks, err := tx.CreateKeyspace("k")
		if err != nil {
			return err
		}
		for i := range 200 {
			if err := ks.Put(fmt.Appendf(nil, "k%04d", i), bytes.Repeat([]byte{'v'}, 100)); err != nil {
				return err
			}
		}
		sks, err := tx.CreateSetKeyspace("s", nil)
		if err != nil {
			return err
		}
		for i := range 400 { // enough members to promote to a nested tree
			if _, err := sks.Put([]byte("setkey"), fmt.Appendf(nil, "member%04d", i)); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return path
}

// checkCodes opens the database and collects the set of issue codes
// Check reports.
func checkCodes(t *testing.T, path string) map[string]int {
	t.Helper()
	ctx := context.Background()
	db, err := Open(ctx, path, Options{
		PageSize: 4096, MinSize: 16, MaxSize: 256,
		PageChecksum: false, // explicit: surgery must not be masked by footers
		Maintenance:  MaintenanceOptions{Disable: true},
	})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db.Close()
	codes := map[string]int{}
	for iss := range db.Check() {
		codes[string(iss.Code)]++
	}
	return codes
}

// surgeon applies fn to the whole file's bytes.
func surgeon(t *testing.T, path string, fn func(data []byte)) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	fn(data)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// TestCheckDetectsOrderingAndCountCorruption pins the tree-level
// consistency classes api-surface.md §Check claims (key ordering,
// separator routing, nested-tree member counts, descriptor counts,
// NumKeyspaces) — none of which per-page checksums or structural
// Validate can see on a checksums-off database.
func TestCheckDetectsOrderingAndCountCorruption(t *testing.T) {
	t.Run("clean-baseline", func(t *testing.T) {
		path := buildConsistencyFixture(t)
		codes := checkCodes(t, path)
		for _, c := range []string{"KeyOrderViolation", "NestedCountMismatch", "KeyspaceCountMismatch", "NumKeyspacesMismatch"} {
			if codes[c] != 0 {
				t.Errorf("clean fixture reports %s x%d", c, codes[c])
			}
		}
	})

	t.Run("leaf-key-order", func(t *testing.T) {
		path := buildConsistencyFixture(t)
		surgeon(t, path, func(data []byte) {
			// Compressed leaves store full keys only at restart
			// points; find any full "k0NNN" occurrence and make it
			// sort above its successors and outside its routing
			// range ('k' -> 'z'). Skip the very first key of the
			// database ("k0000" also appears as a branch separator
			// candidate); any later hit works.
			for i := bytes.Index(data, []byte("k0")); i >= 0; i = bytes.Index(data[i+1:], []byte("k0")) + i + 1 {
				if i+5 <= len(data) &&
					data[i+2] >= '0' && data[i+2] <= '9' &&
					data[i+3] >= '0' && data[i+3] <= '9' &&
					data[i+4] >= '0' && data[i+4] <= '9' &&
					!bytes.Equal(data[i:i+5], []byte("k0000")) {
					data[i] = 'z'
					return
				}
			}
			t.Fatalf("surgery target not found")
		})
		codes := checkCodes(t, path)
		if codes["KeyOrderViolation"] == 0 {
			t.Errorf("no KeyOrderViolation reported; codes=%v", codes)
		}
	})

	t.Run("descriptor-count", func(t *testing.T) {
		path := buildConsistencyFixture(t)
		surgeon(t, path, func(data []byte) {
			// Locate keyspace "k"'s descriptor precisely: read the
			// keyspace-tree root leaf via the meta, find the entry,
			// and bump the descriptor's Count field (offset 8, after
			// Root) at its exact file offset.
			active, ok := page.ActiveMeta(data[:4096], data[4096:8192])
			if !ok {
				t.Fatalf("no valid meta")
			}
			m := page.DecodeMeta(data[active*4096 : (active+1)*4096])
			cfg := page.Config{PageSize: 4096}
			pageStart := int(m.KeyspaceRoot) * 4096
			pageBuf := data[pageStart : pageStart+4096]
			r := page.NewLeafReader(pageBuf, cfg)
			_, e, found := r.SearchLeaf([]byte("k"))
			if !found {
				t.Fatalf("keyspace descriptor entry not found (root not a leaf?)")
			}
			off := bytes.Index(pageBuf, e.Value)
			if off < 0 {
				t.Fatalf("descriptor bytes not located in page")
			}
			data[pageStart+off+8]++ // Count low byte
		})
		codes := checkCodes(t, path)
		if codes["KeyspaceCountMismatch"] == 0 {
			t.Errorf("descriptor-count surgery undetected; codes=%v", codes)
		}
	})

	t.Run("nested-count", func(t *testing.T) {
		path := buildConsistencyFixture(t)
		surgeon(t, path, func(data []byte) {
			// Nested-tree cell layout: [Flags][KeyLen][Key][Root][Count].
			// Locate the "setkey" cell and bump its Count low byte.
			i := bytes.Index(data, []byte("setkey"))
			if i < 0 {
				t.Fatalf("set key not found")
			}
			countOff := i + len("setkey") + 8 // skip Root
			data[countOff]++
		})
		codes := checkCodes(t, path)
		if codes["NestedCountMismatch"] == 0 && codes["KeyspaceCountMismatch"] == 0 {
			t.Errorf("nested-count surgery undetected; codes=%v", codes)
		}
	})

	t.Run("num-keyspaces", func(t *testing.T) {
		path := buildConsistencyFixture(t)
		// Meta pages carry their own checksum regardless of the
		// PageChecksum option — rewrite via EncodeMeta.
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		active, ok := page.ActiveMeta(data[:4096], data[4096:8192])
		if !ok {
			t.Fatalf("no valid meta")
		}
		m := page.DecodeMeta(data[active*4096 : (active+1)*4096])
		m.NumKeyspaces++
		buf := make([]byte, 4096)
		page.EncodeMeta(buf, &m)
		copy(data[active*4096:], buf)
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		codes := checkCodes(t, path)
		if codes["NumKeyspacesMismatch"] == 0 {
			t.Errorf("NumKeyspaces surgery undetected; codes=%v", codes)
		}
	})
}

// TestCheckDetectsDescriptorTreeDisorder pins the ordering pass on a
// tree BEYOND keyspace data roots: the top-level keyspace-descriptor
// tree, where a routing/order flip makes OpenKeyspace descent miss a
// keyspace mid-op while every per-page check stays clean. The index
// registry and index data trees ride the identical validateTreeOrder
// call (their end-to-end surgery is impractical — index keys are
// codec-encoded, not byte-matchable; the mechanism itself is pinned
// by the btree-level ValidateOrder tests).
func TestCheckDetectsDescriptorTreeDisorder(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)
	db, err := Open(ctx, path, Options{
		PageSize: 4096, MinSize: 16, MaxSize: 256,
		PageChecksum: false, // explicit: surgery must not be masked by footers
		Maintenance:  MaintenanceOptions{Disable: true},
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := db.Update(ctx, func(tx *Tx) error {
		for _, name := range []string{"aaa", "bbb", "ccc"} {
			if _, err := tx.CreateKeyspace(name); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	surgeon(t, path, func(data []byte) {
		// Locate keyspace "aaa"'s key bytes inside the descriptor
		// tree's root leaf and flip the first byte high — an
		// intra-leaf order violation in the keyspace tree itself.
		active, ok := page.ActiveMeta(data[:4096], data[4096:8192])
		if !ok {
			t.Fatalf("no valid meta")
		}
		m := page.DecodeMeta(data[active*4096 : (active+1)*4096])
		pageStart := int(m.KeyspaceRoot) * 4096
		pageBuf := data[pageStart : pageStart+4096]
		i := bytes.Index(pageBuf, []byte("aaa"))
		if i < 0 {
			t.Fatalf("keyspace name not found in descriptor leaf")
		}
		pageBuf[i] = 'z'
	})
	codes := checkCodes(t, path)
	if codes["KeyOrderViolation"] == 0 {
		t.Errorf("descriptor-tree disorder undetected; codes=%v", codes)
	}
}

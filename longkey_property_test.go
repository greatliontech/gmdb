package gmdb

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"testing"

	"github.com/greatliontech/gmdb/internal/page"
	"pgregory.net/rapid"
)

// Property harness over the long-key surface (limits.md §Maximum Key
// Size / §Maximum Value Size (Set Keyspaces)): random op sequences on
// a Keyspace and a SetKeyspace with byte-length generators that
// straddle every storage boundary — the inline threshold T (inline vs
// overflow-key form), the subpage promotion budget, and the overflow
// promotion point for plain values — checked against in-memory
// models, across commits, ending with a clean Check.

// lenBucketGen yields lengths biased to the storage boundaries: tiny,
// ordinary, T-straddling (T-1, T, T+1), the subpage window,
// over-budget, and past the subpage encoder's uint16 DataSize cap
// (the routing must be decided by the budget long before that cap —
// a member there once errored out of the encoder instead of going
// nested).
func lenBucketGen(tSz, budget int) *rapid.Generator[int] {
	return rapid.OneOf(
		rapid.IntRange(1, 16),
		rapid.IntRange(17, 500),
		rapid.IntRange(tSz-2, tSz+2),
		rapid.IntRange(tSz+3, budget-8),
		rapid.IntRange(budget-7, budget+900),
		rapid.IntRange(65530, 65545),
		rapid.IntRange(65546, 90000),
	)
}

func genBytes(t *rapid.T, label string, n int) []byte {
	// A random first+fill+last shape: cheap to generate at 4KB-scale
	// lengths while still varying comparisons at both ends (full
	// rapid.SliceOfN at these lengths dominates runtime).
	first := rapid.Byte().Draw(t, label+"-first")
	fill := rapid.Byte().Draw(t, label+"-fill")
	last := rapid.Byte().Draw(t, label+"-last")
	b := bytes.Repeat([]byte{fill}, n)
	b[0] = first
	b[n-1] = last
	return b
}

func TestPropertyLongKeysAcrossSurfaces(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		ctx := context.Background()
		db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 1 << 16,
			Maintenance: MaintenanceOptions{Disable: true}})
		if err != nil {
			rt.Fatalf("Open: %v", err)
		}
		defer db.Close()

		tx, err := db.Begin(ctx)
		if err != nil {
			rt.Fatalf("Begin: %v", err)
		}
		ks, err := tx.CreateKeyspace("kv")
		if err != nil {
			rt.Fatalf("CreateKeyspace: %v", err)
		}
		sks, err := tx.CreateSetKeyspace("set", nil)
		if err != nil {
			rt.Fatalf("CreateSetKeyspace: %v", err)
		}
		cfg := ks.builderCfg()
		tSz := cfg.InlineThreshold()
		budget := page.SubpagePromotionThreshold(cfg)
		lenGen := lenBucketGen(tSz, budget)

		kvModel := map[string][]byte{}   // key -> value
		setModel := map[string]bool{}    // key\x00value membership
		setKeys := map[string]int{}      // key -> member count
		var kvKeys, setPairs [][2][]byte // insertion pools for delete/get probes

		const ops = 40
		for i := range ops {
			switch rapid.IntRange(0, 9).Draw(rt, fmt.Sprintf("op%d", i)) {
			case 0, 1, 2: // KV Put (long keys AND long values)
				k := genBytes(rt, fmt.Sprintf("k%d", i), lenGen.Draw(rt, fmt.Sprintf("klen%d", i)))
				v := genBytes(rt, fmt.Sprintf("v%d", i), lenGen.Draw(rt, fmt.Sprintf("vlen%d", i)))
				if err := ks.Put(k, v); err != nil {
					rt.Fatalf("Put(%d-byte key, %d-byte value): %v", len(k), len(v), err)
				}
				kvModel[string(k)] = v
				kvKeys = append(kvKeys, [2][]byte{k, v})
			case 3: // KV Delete of a known key
				if len(kvKeys) == 0 {
					continue
				}
				k := kvKeys[rapid.IntRange(0, len(kvKeys)-1).Draw(rt, fmt.Sprintf("del%d", i))][0]
				if _, present := kvModel[string(k)]; present {
					if err := ks.Delete(k); err != nil {
						rt.Fatalf("Delete: %v", err)
					}
					delete(kvModel, string(k))
				}
			case 4, 5, 6: // Set Put (long keys AND long members)
				k := genBytes(rt, fmt.Sprintf("sk%d", i), lenGen.Draw(rt, fmt.Sprintf("sklen%d", i)))
				v := genBytes(rt, fmt.Sprintf("sv%d", i), lenGen.Draw(rt, fmt.Sprintf("svlen%d", i)))
				added, err := sks.Put(k, v)
				if err != nil {
					rt.Fatalf("SetPut(%d-byte key, %d-byte member): %v", len(k), len(v), err)
				}
				wasMember := setModel[string(k)+"\x00"+string(v)]
				if added == wasMember {
					rt.Fatalf("SetPut added=%v but model membership=%v", added, wasMember)
				}
				if !wasMember {
					setModel[string(k)+"\x00"+string(v)] = true
					setKeys[string(k)]++
					setPairs = append(setPairs, [2][]byte{k, v})
				}
			case 7: // Set DeleteValue of a known member
				if len(setPairs) == 0 {
					continue
				}
				p := setPairs[rapid.IntRange(0, len(setPairs)-1).Draw(rt, fmt.Sprintf("sdel%d", i))]
				mkey := string(p[0]) + "\x00" + string(p[1])
				if setModel[mkey] {
					if err := sks.DeleteValue(p[0], p[1]); err != nil {
						rt.Fatalf("DeleteValue: %v", err)
					}
					delete(setModel, mkey)
					setKeys[string(p[0])]--
					if setKeys[string(p[0])] == 0 {
						delete(setKeys, string(p[0]))
					}
				}
			case 8: // commit + fresh tx (cross-commit persistence)
				if err := tx.Commit(); err != nil {
					rt.Fatalf("Commit: %v", err)
				}
				tx, err = db.Begin(ctx)
				if err != nil {
					rt.Fatalf("re-Begin: %v", err)
				}
				ks, err = tx.OpenKeyspace("kv")
				if err != nil {
					rt.Fatalf("re-OpenKeyspace: %v", err)
				}
				sks, err = tx.OpenSetKeyspace("set")
				if err != nil {
					rt.Fatalf("re-OpenSetKeyspace: %v", err)
				}
			case 9: // point probes against the models
				if len(kvKeys) > 0 {
					k := kvKeys[rapid.IntRange(0, len(kvKeys)-1).Draw(rt, fmt.Sprintf("get%d", i))][0]
					v, err := ks.Get(k)
					want, present := kvModel[string(k)]
					if present {
						if err != nil || !bytes.Equal(v, want) {
							rt.Fatalf("Get(%d-byte key): err=%v match=%v", len(k), err, bytes.Equal(v, want))
						}
					} else if err == nil {
						rt.Fatalf("Get on deleted key returned a value")
					}
				}
				if len(setPairs) > 0 {
					p := setPairs[rapid.IntRange(0, len(setPairs)-1).Draw(rt, fmt.Sprintf("has%d", i))]
					has, err := sks.HasValue(p[0], p[1])
					if err != nil {
						rt.Fatalf("HasValue: %v", err)
					}
					if has != setModel[string(p[0])+"\x00"+string(p[1])] {
						rt.Fatalf("HasValue=%v disagrees with model", has)
					}
				}
			}
		}
		if err := tx.Commit(); err != nil {
			rt.Fatalf("final Commit: %v", err)
		}

		// Full-scan agreement: the KV cursor yields exactly the model,
		// in order, with full (materialized) keys.
		rtx, err := db.BeginRead(ctx)
		if err != nil {
			rt.Fatalf("BeginRead: %v", err)
		}
		rks, _ := rtx.OpenKeyspaceReadOnly("kv")
		var wantKeys []string
		for k := range kvModel {
			wantKeys = append(wantKeys, k)
		}
		sort.Strings(wantKeys)
		cur := rks.Cursor()
		idx := 0
		for k, v := cur.First(); k != nil; k, v = cur.Next() {
			if idx >= len(wantKeys) {
				rt.Fatalf("cursor yielded more keys than the model (%d)", len(wantKeys))
			}
			if string(k) != wantKeys[idx] {
				rt.Fatalf("cursor key %d: %d bytes, want %d bytes (order/materialization mismatch)",
					idx, len(k), len(wantKeys[idx]))
			}
			if !bytes.Equal(v, kvModel[wantKeys[idx]]) {
				rt.Fatalf("cursor value mismatch at key %d", idx)
			}
			idx++
		}
		if err := cur.Err(); err != nil {
			rt.Fatalf("cursor err: %v", err)
		}
		if idx != len(wantKeys) {
			rt.Fatalf("cursor yielded %d keys, model has %d", idx, len(wantKeys))
		}
		rtx.Rollback()

		// Structural integrity: no leaked extents/runs, clean walk.
		for _, is := range collectIssues(db.Check()) {
			rt.Fatalf("Check issue: %+v", is)
		}
	})
}

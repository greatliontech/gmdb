package gmdb

import (
	"context"
	"fmt"
	"testing"
)

// TestIteratorsUnregisterCursors pins the iterator-registration
// contract: All/Range/Prefix register their cursor while live (the
// loop body may mutate the keyspace and must reach it with the
// staleness broadcast) and unregister on exit — completed or broken —
// so a long transaction alternating iteration with mutation does not
// grow openCursors (and the per-mutation stale walk) unboundedly.
func TestIteratorsUnregisterCursors(t *testing.T) {
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
	ks, err := tx.CreateKeyspace("k")
	if err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	sks, err := tx.CreateSetKeyspace("s", nil)
	if err != nil {
		t.Fatalf("CreateSetKeyspace: %v", err)
	}
	for i := range 20 {
		if err := ks.Put(fmt.Appendf(nil, "k%02d", i), []byte("v")); err != nil {
			t.Fatalf("Put: %v", err)
		}
		if _, err := sks.Put([]byte("set"), fmt.Appendf(nil, "m%02d", i)); err != nil {
			t.Fatalf("set Put: %v", err)
		}
	}

	base := len(ks.openCursors)
	for range 50 {
		for range ks.All() {
		} // completed
		for range ks.Range([]byte("k00"), []byte("k05")) {
			break // early break — the defer must still fire
		}
		n := 0
		for range ks.Prefix([]byte("k1")) {
			if n++; n > 2 {
				break
			}
		}
	}
	if got := len(ks.openCursors); got != base {
		t.Errorf("openCursors grew: %d -> %d after 150 iterations", base, got)
	}

	baseSet := len(sks.openSetCursors)
	for range 50 {
		for range sks.All() {
		}
		for range sks.Range([]byte("set"), nil) {
			break
		}
		for range sks.Prefix([]byte("set")) {
			break
		}
	}
	if got := len(sks.openSetCursors); got != baseSet {
		t.Errorf("openSetCursors grew: %d -> %d after 150 iterations", baseSet, got)
	}

	// The live-registration half: a mutation INSIDE the loop must
	// reach the iterator's cursor via the staleness broadcast —
	// which, per the iterators' documented error model, ENDS the
	// sequence (a staled cursor's Next returns nil; recovery is a
	// fresh iterator). seen == 1 is the deterministic pin: an
	// UNREGISTERED cursor would silently keep walking the retired
	// pre-mutation tree and see every key (a surviving mutation in
	// review round 1 demonstrated exactly that).
	seen := 0
	for k := range ks.All() {
		seen++
		if seen == 1 {
			if err := ks.Put(append([]byte{}, k...), []byte("v2")); err != nil {
				t.Fatalf("mutate during iteration: %v", err)
			}
		}
	}
	if seen != 1 {
		t.Errorf("keyspace iteration saw %d keys after an in-loop mutation, want 1 (stale ends the sequence)", seen)
	}
	if len(ks.openCursors) != base {
		t.Errorf("openCursors after mutate-during-iteration: %d, want %d", len(ks.openCursors), base)
	}
	seenSet := 0
	for k, v := range sks.All() {
		seenSet++
		if seenSet == 1 {
			if _, err := sks.Put(append([]byte{}, k...), append(append([]byte{}, v...), 'x')); err != nil {
				t.Fatalf("set mutate during iteration: %v", err)
			}
		}
	}
	if seenSet != 1 {
		t.Errorf("set iteration saw %d members after an in-loop mutation, want 1", seenSet)
	}
	if len(sks.openSetCursors) != baseSet {
		t.Errorf("openSetCursors after mutate-during-iteration: %d, want %d", len(sks.openSetCursors), baseSet)
	}
}

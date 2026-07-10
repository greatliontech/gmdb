package gmdb

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/thegrumpylion/gmdb/internal/page"
)

// A crash can tear an RPL reclamation between bitmap writebacks: the
// reclaimed segment's ENTRY bits persist as free while the segment's
// OWN bit (and the meta) do not — recovery then adopts a meta whose
// chain still lists the segment, while the adopted bitmap already
// shows its entries free. Check names this FreeAndPending: the entries
// can be re-allocated into the live tree and later "reclaimed" a
// second time under their new owner — double allocation, silent
// corruption (free-space.md §RPL reclamation, crash coherence).
// attachState must RE-ARM the torn state before installing the chain:
// every free bit on a still-pending entry is cleared back to
// allocated (position-independent across arbitrary per-segment tear
// subsets) and the touched bitmap pages are persisted immediately so
// a post-crash Check is clean.
func TestCrashTornReclamationRearmedAtOpen(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)
	opts := Options{
		PageSize: 4096, MinSize: 16, MaxSize: 512,
		Maintenance: MaintenanceOptions{Disable: true},
	}
	db, err := Open(ctx, path, opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	rec := &crashRecorder{inner: db.WriterFileOpsForTest()}
	restore := db.SetWriterFileOpsForTest(rec)
	preRecorder, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read pre-recorder image: %v", err)
	}

	// Rounds of put+delete grow the RPL; under SyncDurable the bound
	// keeps pace, so an aged tail segment reclaims at a later commit's
	// start. Detect the reclaiming commit by the tail advancing.
	live := map[string][]byte{}
	commit := func(round int) {
		tx, err := db.Begin(ctx)
		if err != nil {
			t.Fatalf("Begin(%d): %v", round, err)
		}
		ks, err := tx.OpenKeyspace("k")
		if err != nil {
			if ks, err = tx.CreateKeyspace("k"); err != nil {
				t.Fatalf("CreateKeyspace: %v", err)
			}
		}
		for i := range 30 {
			k := fmt.Sprintf("r%03d-%03d", round, i)
			v := bytes.Repeat([]byte{byte('A' + round%26)}, 500)
			if err := ks.Put([]byte(k), v); err != nil {
				t.Fatalf("Put: %v", err)
			}
			live[k] = v
		}
		if round > 0 {
			for i := range 20 {
				k := fmt.Sprintf("r%03d-%03d", round-1, i)
				if err := ks.Delete([]byte(k)); err != nil {
					t.Fatalf("Delete: %v", err)
				}
				delete(live, k)
			}
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("Commit(%d): %v", round, err)
		}
		rec.mark()
	}

	// Phase 1: grow a multi-segment chain. Phase 2: MINIMAL probe
	// commits (one tiny in-place put → a couple of CoW pages) so the
	// reclaiming commit reuses almost none of the pages it frees —
	// otherwise its own allocations re-clear the entry bits and the
	// half-reclaimed window closes.
	var reclaimedSeg uint64
	var beforeMark, afterMark int
	var liveAtCrash map[string][]byte
	round := 0
	for ; round < 60; round++ {
		commit(round)
		m := db.Meta()
		if m.RPLTailPage != 0 && m.RPLHeadPage != 0 && m.RPLHeadPage != m.RPLTailPage {
			break
		}
	}
	if db.Meta().RPLTailPage == 0 {
		t.Fatal("fixture: no RPL chain built")
	}
	// Pin a reader so reclamation is blocked while several more
	// segments age past the anchor — then release it. The next
	// tx-start reclaims them ALL in one pass: the mixed multi-segment
	// tear is the shape that broke the tail-contiguity design.
	pinRead, err := db.BeginRead(ctx)
	if err != nil {
		t.Fatalf("BeginRead: %v", err)
	}
	for extra := 0; extra < 4; extra++ {
		round++
		commit(round)
	}
	if err := pinRead.Rollback(); err != nil {
		t.Fatalf("release reader: %v", err)
	}
	probe := func(n int) {
		tx, err := db.Begin(ctx)
		if err != nil {
			t.Fatalf("probe Begin(%d): %v", n, err)
		}
		ks, err := tx.OpenKeyspace("k")
		if err != nil {
			t.Fatalf("probe OpenKeyspace: %v", err)
		}
		k := fmt.Sprintf("r%03d-%03d", round, 0) // existing key, in-place replace
		if err := ks.Put([]byte(k), bytes.Repeat([]byte{'P'}, 200)); err != nil {
			t.Fatalf("probe Put: %v", err)
		}
		live[k] = bytes.Repeat([]byte{'P'}, 200)
		if err := tx.Commit(); err != nil {
			t.Fatalf("probe Commit(%d): %v", n, err)
		}
		rec.mark()
	}
	prevTail := db.Meta().RPLTailPage
	for n := range 10 {
		snapshot := make(map[string][]byte, len(live))
		for k, v := range live {
			snapshot[k] = v
		}
		probe(n)
		m := db.Meta()
		if m.RPLTailPage != prevTail {
			reclaimedSeg = prevTail
			rec.mu.Lock()
			afterMark = rec.marks[len(rec.marks)-1]
			beforeMark = rec.marks[len(rec.marks)-2]
			rec.mu.Unlock()
			liveAtCrash = snapshot
			break
		}
		prevTail = m.RPLTailPage
	}
	restore()
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if reclaimedSeg == 0 {
		t.Fatal("fixture: no probe commit reclaimed the tail")
	}

	// Crash image: everything before the reclaiming commit, plus the
	// commit's writes EXCEPT (a) the byte carrying each reclaimed
	// segment's own bitmap bit is reverted (a prefix tear of the
	// bitmap-page write) and (b) the meta-page writes are dropped
	// (recovery adopts the pre-reclamation meta). Both are legitimate
	// members of the crash-subset family the durability spec models.
	rec.mu.Lock()
	ops := rec.ops
	rec.mu.Unlock()
	initial := synthImage(preRecorder, ops, beforeMark)
	img := synthImage(preRecorder, ops, beforeMark)
	pageSize := int64(4096)
	// The probe may reclaim SEVERAL aged segments in one pass. The
	// dangerous crash shape keeps every reclaimed segment IN the
	// adopted chain: each segment page's own bitmap bit and its
	// reuse-write must not persist, while the entry bits do. Identify
	// reclaimed segment pages: bits that flipped allocated→free in the
	// probe whose pre-probe content is an RPL segment page.
	segPages := map[uint64]bool{reclaimedSeg: true}
	// Scan candidate ids covered by the probe's bitmap writes.
	for _, op := range ops[beforeMark:afterMark] {
		if op.kind != crashOpWrite || op.off != 2*pageSize {
			continue
		}
		for i, b := range op.data {
			pre := initial[2*pageSize+int64(i)]
			for bit := 0; bit < 8; bit++ {
				m := byte(1) << bit
				if b&m != 0 && pre&m == 0 { // flipped to free
					id := uint64(i*8 + bit)
					off := int64(id) * pageSize
					if off+1 <= int64(len(initial)) && initial[off] == byte(page.TypeRPLSegment) {
						segPages[id] = true
					}
				}
			}
		}
	}
	for _, op := range ops[beforeMark:afterMark] {
		if op.kind != crashOpWrite {
			continue
		}
		if op.off == 0 || op.off == pageSize {
			continue // meta pages: not persisted
		}
		if segPages[uint64(op.off)/uint64(pageSize)] && op.off%pageSize == 0 {
			// Reuse-writes to reclaimed segment pages: not persisted —
			// the old segment content survives under the adopted chain.
			continue
		}
		end := op.off + int64(len(op.data))
		if int64(len(img)) < end {
			img = append(img, make([]byte, end-int64(len(img)))...)
		}
		copy(img[op.off:end], op.data)
	}
	// Revert the whole BYTE carrying each reclaimed segment page's own
	// bit — byte granularity matches the harness's declared prefix-tear
	// model of partial page writes (a lone-bit revert would not be
	// producible by any write subset). Neighboring entry bits in the
	// same byte revert with it — the ≥3-free-entries guard below keeps
	// the fixture meaningful.
	for id := range segPages {
		byteOff := 2*pageSize + int64(id/8)
		img[byteOff] = initial[byteOff]
	}

	// Fixture guard: the image must actually carry the half-reclaimed
	// state — the segment in the adopted chain with a meaningful number
	// of its entries already free — or the test pins nothing. Count
	// free bits among the segment's decoded entries.
	{
		segOff := int64(reclaimedSeg) * pageSize
		entryCount := int(uint16(img[segOff+2]) | uint16(img[segOff+3])<<8)
		freeEntries := 0
		for i := range entryCount {
			eOff := segOff + 24 + int64(i)*8 // entries start after the 24-byte segment header
			id := uint64(img[eOff]) | uint64(img[eOff+1])<<8 | uint64(img[eOff+2])<<16 | uint64(img[eOff+3])<<24
			byteOff := 2*pageSize + int64(id/8)
			if img[byteOff]&(1<<(id%8)) != 0 {
				freeEntries++
			}
		}
		if freeEntries < 3 {
			t.Fatalf("fixture drifted: only %d of %d reclaimed-segment entries are free in the crash image (need the half-reclaimed window)", freeEntries, entryCount)
		}
	}
	if len(segPages) < 2 {
		t.Fatalf("fixture drifted: only %d reclaimed segments in the crash image — the multi-segment mixed tear is unpinned", len(segPages))
	}

	crashPath := tmpPath(t)
	if err := os.WriteFile(crashPath, img, 0o600); err != nil {
		t.Fatalf("write crash image: %v", err)
	}
	// Idempotence: the first Open re-arms and persists WITHOUT any
	// commit; a second Open of the resulting file must be a clean
	// no-op (re-crash-before-first-commit semantics).
	db2a, err := Open(ctx, crashPath, opts)
	if err != nil {
		t.Fatalf("re-Open crash image: %v", err)
	}
	if err := db2a.Close(); err != nil {
		t.Fatalf("Close after first recovery: %v", err)
	}
	db2, err := Open(ctx, crashPath, opts)
	if err != nil {
		t.Fatalf("second re-Open (idempotence): %v", err)
	}
	defer db2.Close()

	// (a) The half-reclaimed state must be gone: no FreeAndPending /
	// ReachableButFree from Check.
	for iss := range db2.Check() {
		if iss.Code == "FreeAndPending" || iss.Code == "ReachableButFree" {
			t.Fatalf("Check after recovery: %+v (crashed reclamation not completed at Open)", iss)
		}
	}
	// (b) End-to-end: hammer allocations so freed pages get reused,
	// then verify every surviving key — a double allocation would
	// corrupt values or fail reads.
	for round := 100; round < 130; round++ {
		tx, err := db2.Begin(ctx)
		if err != nil {
			t.Fatalf("hammer Begin(%d): %v", round, err)
		}
		ks, err := tx.OpenKeyspace("k")
		if err != nil {
			t.Fatalf("hammer OpenKeyspace: %v", err)
		}
		for i := range 20 {
			k := fmt.Sprintf("h%03d-%03d", round, i)
			v := bytes.Repeat([]byte{byte('a' + round%26)}, 200)
			if err := ks.Put([]byte(k), v); err != nil {
				t.Fatalf("hammer Put: %v", err)
			}
			liveAtCrash[k] = v
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("hammer Commit(%d): %v", round, err)
		}
	}
	rtx, _ := db2.Begin(ctx)
	defer rtx.Rollback()
	rks, err := rtx.OpenKeyspace("k")
	if err != nil {
		t.Fatalf("verify OpenKeyspace: %v", err)
	}
	for k, want := range liveAtCrash {
		got, gerr := rks.Get([]byte(k))
		if gerr != nil {
			t.Fatalf("Get(%q) after hammer: %v", k, gerr)
		}
		if got != nil && !bytes.Equal(got, want) {
			t.Fatalf("Get(%q) = %d bytes of %q, want %q — page reused under a live reference",
				k, len(got), got[:1], want[:1])
		}
	}
	rtx.Rollback()
	// BitmapLeak WARNINGS are expected here: the crashed commit's
	// persisted data writes orphan allocations that only background
	// maintenance (disabled in this fixture) reclaims — they are not
	// re-arm stranding. The error-severity bar plus the hammer's
	// read-back is what pins the re-armed pages' lifecycle.
	for iss := range db2.Check() {
		if iss.Severity == CheckError || iss.Severity == CheckFatal {
			t.Fatalf("final Check: %+v", iss)
		}
	}
}

package gmdb

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
)

// TestMaintenanceReclaimDiscardsStaleDetection pins the
// snapshot-currency guard (background-maintenance.md §Bitmap Leak
// Reclamation): leak detection classifies against the SNAPSHOT tree
// but the LIVE bitmap, so a commit landing inside the detection
// window makes freshly-allocated live pages classify as leaked —
// without the guard the reclamation tx frees them out from under the
// live tree (the audit demonstrated 6 such pages, 5 ReachableButFree
// after reclamation). The reclamation tx must observe the TxnID
// advance and discard the whole set.
func TestMaintenanceReclaimDiscardsStaleDetection(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{
		PageSize: 4096, MinSize: 16, MaxSize: 512,
		Maintenance: MaintenanceOptions{Disable: true}, // passes driven manually
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	// Seed a wide tree, then range-delete most of it so the bitmap
	// holds free holes below the high-water mark.
	if err := db.Update(ctx, func(tx *Tx) error {
		ks, err := tx.CreateKeyspace("k")
		if err != nil {
			return err
		}
		for i := range 800 {
			if err := ks.Put(fmt.Appendf(nil, "k%04d", i), bytes.Repeat([]byte{'v'}, 200)); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := db.Update(ctx, func(tx *Tx) error {
		ks, err := tx.OpenKeyspace("k")
		if err != nil {
			return err
		}
		_, err = ks.DeleteRange([]byte("k0000"), []byte("k0700"))
		return err
	}); err != nil {
		t.Fatalf("DeleteRange: %v", err)
	}
	// Two follow-up commits advance the reclamation bound past the
	// DeleteRange's RPL segment, so its pages become genuinely FREE
	// holes below the high-water mark before detection begins — the
	// precondition for the misclassification (a page free at snapshot
	// time, allocated by the racing commit).
	for i := range 2 {
		if err := db.Update(ctx, func(tx *Tx) error {
			ks, err := tx.OpenKeyspace("k")
			if err != nil {
				return err
			}
			return ks.Put(fmt.Appendf(nil, "tick%d", i), []byte("t"))
		}); err != nil {
			t.Fatalf("tick %d: %v", i, err)
		}
	}

	// The racing commit fires inside the detection window: it
	// re-allocates the free holes into the live tree, so detection —
	// walking the OLD snapshot tree but accounting against the NEW
	// bitmap — classifies those live pages as leaked.
	live := map[string]bool{}
	restore := SetMaintDetectHookForTest(func() {
		if err := db.Update(ctx, func(tx *Tx) error {
			ks, err := tx.OpenKeyspace("k")
			if err != nil {
				return err
			}
			for i := range 300 {
				k := fmt.Sprintf("new%04d", i)
				if err := ks.Put([]byte(k), bytes.Repeat([]byte{'n'}, 200)); err != nil {
					return err
				}
				live[k] = true
			}
			return nil
		}); err != nil {
			t.Errorf("racing commit: %v", err)
		}
	})
	freed, discarded := db.MaintReclaimLeaksForTest(ctx)
	restore()
	if !discarded {
		t.Fatalf("guard did not fire (freed=%d): the racing commit must stale the detection (vacuity guard)", freed)
	}

	// The live tree must be fully intact: every racing-commit row
	// readable, Check clean (without the guard: ReachableButFree /
	// corrupted reads).
	rtx, err := db.BeginRead(ctx)
	if err != nil {
		t.Fatalf("BeginRead: %v", err)
	}
	defer rtx.Rollback()
	ks, err := rtx.OpenKeyspaceReadOnly("k")
	if err != nil {
		t.Fatalf("OpenKeyspaceReadOnly: %v", err)
	}
	for k := range live {
		if _, err := ks.Get([]byte(k)); err != nil {
			t.Fatalf("Get(%s) after guarded pass: %v", k, err)
		}
	}
	for i := 700; i < 800; i++ {
		if _, err := ks.Get(fmt.Appendf(nil, "k%04d", i)); err != nil && !errors.Is(err, ErrNotFound) {
			t.Fatalf("Get(k%04d): %v", i, err)
		}
	}
	for iss := range db.Check() {
		if iss.Severity == CheckError || iss.Severity == CheckFatal || iss.Code == "BitmapLeak" {
			t.Errorf("Check issue after guarded pass: %+v", iss)
		}
	}

	// A quiet follow-up pass (no racing commit) must not discard and
	// must leave the database clean — this fixture has no genuine
	// leaks (nothing crashed), so zero BitmapLeak warnings expected.
	if _, discarded := db.MaintReclaimLeaksForTest(ctx); discarded {
		t.Fatalf("quiet pass discarded — TxnID/pager guard misfiring")
	}
	for iss := range db.Check() {
		if iss.Severity == CheckError || iss.Severity == CheckFatal || iss.Code == "BitmapLeak" {
			t.Errorf("Check issue after quiet pass: %+v", iss)
		}
	}
}

// TestMaintenanceReclaimDiscardsAfterCompact pins the guard's
// pager-identity term: a same-process Compact between detection and
// reclamation rebuilds the file densely and RESETS TxnID, so with
// realigning commits the TxnID term alone would spuriously pass while
// every detected id names a different page. The identity term must
// discard.
func TestMaintenanceReclaimDiscardsAfterCompact(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{
		PageSize: 4096, MinSize: 16, MaxSize: 512,
		Maintenance: MaintenanceOptions{Disable: true},
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	if err := db.Update(ctx, func(tx *Tx) error {
		ks, err := tx.CreateKeyspace("k")
		if err != nil {
			return err
		}
		for i := range 800 {
			if err := ks.Put(fmt.Appendf(nil, "k%04d", i), bytes.Repeat([]byte{'v'}, 200)); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := db.Update(ctx, func(tx *Tx) error {
		ks, err := tx.OpenKeyspace("k")
		if err != nil {
			return err
		}
		_, err = ks.DeleteRange([]byte("k0000"), []byte("k0700"))
		return err
	}); err != nil {
		t.Fatalf("DeleteRange: %v", err)
	}
	for i := range 2 {
		if err := db.Update(ctx, func(tx *Tx) error {
			ks, err := tx.OpenKeyspace("k")
			if err != nil {
				return err
			}
			return ks.Put(fmt.Appendf(nil, "tick%d", i), []byte("t"))
		}); err != nil {
			t.Fatalf("tick %d: %v", i, err)
		}
	}

	// Racing commit inside detection → non-empty misclassified set.
	restoreDetect := SetMaintDetectHookForTest(func() {
		if err := db.Update(ctx, func(tx *Tx) error {
			ks, err := tx.OpenKeyspace("k")
			if err != nil {
				return err
			}
			for i := range 300 {
				if err := ks.Put(fmt.Appendf(nil, "new%04d", i), bytes.Repeat([]byte{'n'}, 200)); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Errorf("racing commit: %v", err)
		}
	})
	defer restoreDetect()

	// Between detection and reclamation: Compact (TxnID resets to a
	// fresh counter) then realigning tick commits so the current
	// TxnID equals the detection snapshot's — only the identity term
	// can catch this.
	detTxn := db.Meta().TxnID // the pass's detection snapshot TxnID (racing commit lands after BeginRead)
	restorePre := SetMaintPreReclaimHookForTest(func() {
		if err := db.Compact(); err != nil {
			t.Errorf("Compact: %v", err)
			return
		}
		for db.Meta().TxnID < detTxn {
			if err := db.Update(ctx, func(tx *Tx) error {
				ks, err := tx.OpenKeyspace("k")
				if err != nil {
					return err
				}
				return ks.Put([]byte("realign"), []byte(fmt.Sprint(db.Meta().TxnID)))
			}); err != nil {
				t.Errorf("realign commit: %v", err)
				return
			}
		}
		if got := db.Meta().TxnID; got != detTxn {
			t.Errorf("realign overshoot: TxnID %d, want %d", got, detTxn)
		}
	})
	defer restorePre()

	_, discarded := db.MaintReclaimLeaksForTest(ctx)
	restorePre()
	restoreDetect()
	if !discarded {
		t.Fatalf("guard did not discard after Compact + TxnID realignment (identity term dead)")
	}
	for iss := range db.Check() {
		if iss.Severity == CheckError || iss.Severity == CheckFatal || iss.Code == "BitmapLeak" {
			t.Errorf("Check issue after discarded pass: %+v", iss)
		}
	}
}

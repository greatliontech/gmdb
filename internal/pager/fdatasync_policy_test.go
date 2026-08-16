package pager

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/greatliontech/gmdb/internal/page"
)

// TestOpenInstallsBarrierPolicy pins the Options.NoFullFsync plumbing:
// the flag handed to Open must land in the writer's FileOps seam, and
// the zero value must keep full-strength barriers. The barrier choice
// itself is behaviorally observable only on darwin (linux fdatasync
// ignores the flag), so the wiring is pinned structurally here and the
// darwin behavior is exercised by the platform test suite.
func TestOpenInstallsBarrierPolicy(t *testing.T) {
	for _, noFull := range []bool{false, true} {
		f, err := os.OpenFile(filepath.Join(t.TempDir(), "db.gmdb"),
			os.O_RDWR|os.O_CREATE, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		ip := InitParams{
			PageSize: testPageSize, MinSize: 16, MaxSize: 128,
			GrowStep: 4, ShrinkThreshold: 8, NoFullFsync: noFull,
		}
		if err := Init(f, ip); err != nil {
			t.Fatalf("Init: %v", err)
		}
		opened, err := Open(f, OpenParams{
			Pool:             NewBufPool(testPageSize),
			MaxTxBufferBytes: 16 << 20,
			NoFullFsync:      noFull,
		})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		fops, ok := opened.Pager.fops.(osFileOps)
		if !ok {
			t.Fatalf("fops = %T, want osFileOps", opened.Pager.fops)
		}
		if fops.noFullFsync != noFull {
			t.Errorf("writer fops.noFullFsync = %v, want %v", fops.noFullFsync, noFull)
		}
		_ = opened.Pager.Close()
	}
}

// TestReaderKeepsFullBarrierDefault pins that reader pagers — which
// issue no barriers in production — carry the safe full-strength
// default in their seam rather than any opt-down.
func TestReaderKeepsFullBarrierDefault(t *testing.T) {
	f, err := os.OpenFile(filepath.Join(t.TempDir(), "r.gmdb"),
		os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := f.Truncate(4 * testPageSize); err != nil {
		t.Fatal(err)
	}
	p, err := NewReader(f, page.Config{PageSize: testPageSize}, 4*testPageSize)
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	defer p.Close()
	fops, ok := p.fops.(osFileOps)
	if !ok {
		t.Fatalf("fops = %T, want osFileOps", p.fops)
	}
	if fops.noFullFsync {
		t.Error("reader fops.noFullFsync = true, want false (safe default)")
	}
}

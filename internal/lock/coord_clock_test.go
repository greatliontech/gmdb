package lock

import (
	"context"
	"os"
	"testing"
)

// TestCoordClockNeverStampsSentinel enforces the heartbeat-sentinel
// invariant (cross-process.md): a STAMPED heartbeat is never the
// literal 0 that means "unstamped/cleared". The Coord clock funnel is
// floored at 1 ns, so even a clock legitimately reading 0 — the boot
// instant, exact under a virtualized boot clock — stamps 1.
func TestCoordClockNeverStampsSentinel(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	f, err := Open(OpenParams{Root: root, Base: "x.lock", DataUUID: [16]byte{1}, MaxReaders: MinMaxReaders})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer f.Close()
	c := NewCoord(f, CoordOptions{PID: 42, Clock: func() uint64 { return 0 }})
	defer c.Close()
	if got := c.clock(); got != 1 {
		t.Fatalf("floored coordination clock = %d, want 1", got)
	}
	idx, gen, err := c.AcquireReader(context.Background(), 1)
	if err != nil {
		t.Fatalf("AcquireReader: %v", err)
	}
	if hb := Load64(&f.Slot(idx).Heartbeat); hb != 1 {
		t.Fatalf("stamped heartbeat = %d, want 1 (a stamp must never be the 0 sentinel)", hb)
	}
	c.ReleaseReader(idx, gen)
}

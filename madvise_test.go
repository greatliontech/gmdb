package gmdb

import (
	"context"
	"testing"
)

// TestMadviseOptionsSmoke is the acceptance for the mmap-tuning Options
// (PreloadPages / HugePages / ReclaimOnClose, mmap-strategy.md): each
// opt-in mmap tuning knob (and all three together) must be tolerated at
// open / read-close and must not affect read correctness. The advice
// calls themselves are advisory and not directly observable, so the
// smoke test asserts the wiring runs end-to-end and the committed value
// still reads back. ReclaimOnClose additionally exercises the per-tx
// accessed-range tracking (every Page read during the View) and the
// MADV_COLD issued by the read-tx close path.
func TestMadviseOptionsSmoke(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name string
		opts Options
	}{
		{"PreloadPages", Options{PreloadPages: true}},
		{"HugePages", Options{HugePages: true}},
		{"ReclaimOnClose", Options{ReclaimOnClose: true}},
		{"AllThree", Options{PreloadPages: true, HugePages: true, ReclaimOnClose: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := tmpPath(t)
			seedDB(t, path) // helper from read_only_test.go (same package)

			db, err := Open(ctx, path, tc.opts)
			if err != nil {
				t.Fatalf("Open(%+v): %v", tc.opts, err)
			}
			defer db.Close()

			// Read under the tuning options — must be tolerated + correct.
			readHello(t, db)
			// A second read so ReclaimOnClose's close-path MADV_COLD runs
			// against a freshly-tracked tx more than once.
			readHello(t, db)
		})
	}
}

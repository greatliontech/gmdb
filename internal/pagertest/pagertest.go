// Package pagertest provides the canonical writer-pager test fixture
// for CROSS-PACKAGE tests (internal/btree's pager-integration tests,
// future keyspace-on-real-pager tests). It exists to kill per-package
// fixture duplication: parameter drift between copies produced two
// silently different fixtures once already.
//
// internal/pager's own white-box tests keep a package-private twin
// (setupWriter): this package imports pager, so package-pager test
// files cannot import it back (cycle). That twin is white-box-local;
// THIS fixture is the one cross-package consumers share.
package pagertest

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/thegrumpylion/gmdb/internal/bitmap"
	"github.com/thegrumpylion/gmdb/internal/page"
	"github.com/thegrumpylion/gmdb/internal/pager"
)

// Params configures SetupWriter. Zero values: PageSize 4096,
// RestartGroupTarget 0 (uncompressed-leaning default per
// page.Config), MarkAllFree false (and with it the empty-file shape:
// HWM at firstDataPage, nothing free — allocation extends the file).
type Params struct {
	Pages              int
	PageSize           uint32
	RestartGroupTarget uint16
	// MarkAllFree sets every data page's bitmap bit free and (forced
	// with it) HWMAtTop — the exhaustion-testing shape: with the HWM
	// at the top of the space, draining the bitmap yields ErrDBFull
	// instead of falling to file extension, which would re-hand-out
	// firstDataPage (a duplicate of the first bitmap allocation). A
	// real pager never reaches free-bits-above-HWM, so the two knobs
	// travel together.
	MarkAllFree bool
}

// SetupWriter creates a truncated temp file and an attached writer
// pager over it per p. The caller owns Close on both returns.
func SetupWriter(tb testing.TB, p Params) (*pager.Pager, *bitmap.Bitmap, *os.File) {
	tb.Helper()
	if p.PageSize == 0 {
		p.PageSize = 4096
	}
	dir := tb.TempDir()
	path := filepath.Join(dir, "db.gmdb")
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		tb.Fatalf("pagertest: open: %v", err)
	}
	if err := f.Truncate(int64(p.Pages) * int64(p.PageSize)); err != nil {
		tb.Fatalf("pagertest: truncate: %v", err)
	}
	pool := pager.NewBufPool(int(p.PageSize))
	cfg := page.Config{PageSize: p.PageSize, RestartGroupTarget: p.RestartGroupTarget}
	pg, err := pager.NewWriter(f, cfg, int64(p.Pages)*int64(p.PageSize), pool, 16<<20)
	if err != nil {
		tb.Fatalf("pagertest: NewWriter: %v", err)
	}
	bm := bitmap.New(make([]byte, p.PageSize), p.PageSize, 1, uint64(p.Pages))
	pg.AttachBitmap(bm)
	hwm := bm.FirstDataPage()
	if p.MarkAllFree {
		hwm = uint64(p.Pages)
		for id := bm.FirstDataPage(); id < uint64(p.Pages); id++ {
			bm.Set(id)
		}
	}
	pg.SetCommitState(hwm, uint64(p.Pages), 0)
	return pg, bm, f
}

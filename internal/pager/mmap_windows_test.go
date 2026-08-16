//go:build windows

package pager

import (
	"os"
	"path/filepath"
	"testing"
)

// TestWindowsMappingLifecycle walks the placeholder mapping through
// its full life — map, grow twice (view extensions), shrink into a
// former view (tail unmap + truncate + prefix remap), read at every
// stage, teardown — asserting each transition's error, and finally
// that the file is DELETABLE: a mapping the teardown failed to
// release pins the file name on windows, which is exactly how a
// silent unmap failure surfaces in the wider suite (TempDir cleanup
// denials). The unix suite cannot execute this seam; this test is the
// windows soak's ground truth for mmap-strategy.md §Windows.
//
// mmapProbeSink defeats dead-load elision: each stage read must touch
// the mapped page for real, or a mis-mapped view would go unnoticed.
var mmapProbeSink byte

func TestWindowsMappingLifecycle(t *testing.T) {
	const G = allocGranularity
	f, err := os.Create(filepath.Join(t.TempDir(), "m"))
	if err != nil {
		t.Fatal(err)
	}
	if err := platformTruncate(f, G); err != nil {
		t.Fatalf("initial truncate: %v", err)
	}

	m, err := mmapRO(f.Fd(), 8*G)
	if err != nil {
		t.Fatalf("mmapRO: %v", err)
	}
	mmapProbeSink += m[G-1] // real load — a mis-mapped view faults here

	if err := platformTruncate(f, 3*G); err != nil {
		t.Fatalf("grow truncate to 3G: %v", err)
	}
	if err := mmapEnsureCoverage(m, f.Fd(), 3*G); err != nil {
		t.Fatalf("ensure coverage 3G: %v", err)
	}
	mmapProbeSink += m[3*G-1]

	if err := platformTruncate(f, 5*G); err != nil {
		t.Fatalf("grow truncate to 5G: %v", err)
	}
	if err := mmapEnsureCoverage(m, f.Fd(), 5*G); err != nil {
		t.Fatalf("ensure coverage 5G: %v", err)
	}
	mmapProbeSink += m[5*G-1]

	// Shrink to 2G — inside the second view: pops two views, then the
	// truncation, then the surviving-prefix remap.
	if err := mmapPrepareShrink(m, f.Fd(), 2*G); err != nil {
		t.Fatalf("prepareShrink to 2G: %v", err)
	}
	if err := platformTruncate(f, 2*G); err != nil {
		t.Fatalf("shrink truncate to 2G: %v", err)
	}
	if err := mmapEnsureCoverage(m, f.Fd(), 2*G); err != nil {
		t.Fatalf("ensure coverage post-shrink: %v", err)
	}
	mmapProbeSink += m[2*G-1]

	if err := munmap(m); err != nil {
		t.Fatalf("munmap: %v", err)
	}
	name := f.Name()
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(name); err != nil {
		t.Fatalf("file undeletable after full teardown — a mapping leaked: %v", err)
	}
}

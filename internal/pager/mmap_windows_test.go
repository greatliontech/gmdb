//go:build windows

package pager

import (
	"os"
	"path/filepath"
	"testing"
)

// mmapProbeSink defeats dead-load elision: each stage read must touch
// the mapped page for real, or a mis-mapped view would go unnoticed.
var mmapProbeSink byte

// lifecycleFixture opens a fresh file of the given length and maps it
// with the given reservation, registering crash-safe teardown: a
// t.Fatal at any stage must not leave the mapping or handle alive, or
// the cascade (a pinned TempDir) would mask the real failure.
func lifecycleFixture(t *testing.T, fileLen, reservation int64) (*os.File, []byte) {
	t.Helper()
	f, err := os.Create(filepath.Join(t.TempDir(), "m"))
	if err != nil {
		t.Fatal(err)
	}
	if err := platformTruncate(f, fileLen); err != nil {
		f.Close()
		t.Fatalf("initial truncate: %v", err)
	}
	m, err := mmapRO(f.Fd(), reservation)
	if err != nil {
		f.Close()
		t.Fatalf("mmapRO: %v", err)
	}
	t.Cleanup(func() {
		// Best-effort: after a successful removeAsserting these are
		// double-teardowns whose errors are meaningless; after a
		// mid-test Fatal they are what keeps the TempDir removable.
		_ = munmap(m)
		_ = f.Close()
	})
	return f, m
}

// removeAsserting tears the mapping and handle down through the REAL
// teardown path and asserts the file is then deletable: a mapping the
// teardown failed to release pins the file name on windows, which is
// exactly how a silent unmap failure surfaces in the wider suite
// (TempDir cleanup denials).
func removeAsserting(t *testing.T, f *os.File, m []byte) {
	t.Helper()
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

// TestWindowsMappingLifecycle walks the placeholder mapping
// (mmap-strategy.md §Windows) through its distinct lives, asserting
// every transition's error and, in each scenario, final
// deletability. The unix suite cannot execute this seam; these
// scenarios are the windows soak's ground truth.
func TestWindowsMappingLifecycle(t *testing.T) {
	const G = allocGranularity

	// The common whole-suite shape: file at its final size from the
	// start (one view, no split), reads, teardown.
	t.Run("single view", func(t *testing.T) {
		f, m := lifecycleFixture(t, 2*G, 2*G)
		mmapProbeSink += m[2*G-1]
		removeAsserting(t, f, m)
	})

	// Growth only: two view extensions, teardown of a multi-view
	// mapping.
	t.Run("grow", func(t *testing.T) {
		f, m := lifecycleFixture(t, G, 8*G)
		mmapProbeSink += m[G-1]
		for _, to := range []int64{3 * G, 5 * G} {
			if err := platformTruncate(f, to); err != nil {
				t.Fatalf("grow truncate to %d: %v", to, err)
			}
			if err := mmapEnsureCoverage(m, f.Fd(), to); err != nil {
				t.Fatalf("ensure coverage %d: %v", to, err)
			}
			mmapProbeSink += m[to-1]
		}
		removeAsserting(t, f, m)
	})

	// Shrink: windows refuses SetEndOfFile while ANY view exists, so
	// the shrink protocol is full unmap → truncate → remap [0,target)
	// at the same base.
	t.Run("grow then shrink", func(t *testing.T) {
		f, m := lifecycleFixture(t, G, 8*G)
		for _, to := range []int64{3 * G, 5 * G} {
			if err := platformTruncate(f, to); err != nil {
				t.Fatalf("grow truncate to %d: %v", to, err)
			}
			if err := mmapEnsureCoverage(m, f.Fd(), to); err != nil {
				t.Fatalf("ensure coverage %d: %v", to, err)
			}
		}
		mmapProbeSink += m[5*G-1]
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
		removeAsserting(t, f, m)
	})
}

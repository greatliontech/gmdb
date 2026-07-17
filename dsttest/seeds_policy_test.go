//go:build dst

package dsttest

import "testing"

// TestSweepSeedsPolicy pins the DST_SEEDS contract (docs/specs/
// dst-testing.md §Seed policy): anchors always run; "+N" appends that
// many consecutive seeds from the fixed base; an explicit list — or a
// single bare seed value, the reported-seed paste case — appends
// exactly those seeds; garbage fails loud.
func TestSweepSeedsPolicy(t *testing.T) {
	t.Setenv("DST_SEEDS", "")
	if got := sweepSeeds(t, 7, 8); len(got) != 2 || got[0] != 7 || got[1] != 8 {
		t.Fatalf("no env: %v", got)
	}
	t.Setenv("DST_SEEDS", "+3")
	got := sweepSeeds(t, 7)
	want := []uint64{7, extraSeedBase, extraSeedBase + 1, extraSeedBase + 2}
	if len(got) != len(want) {
		t.Fatalf("count form: %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("count form: %v, want %v", got, want)
		}
	}
	t.Setenv("DST_SEEDS", "41, 42")
	got = sweepSeeds(t, 7)
	if len(got) != 3 || got[1] != 41 || got[2] != 42 {
		t.Fatalf("list form: %v", got)
	}
	// A bare number is a seed VALUE — the reported-seed paste case the
	// "+" prefix exists to keep unambiguous.
	t.Setenv("DST_SEEDS", "1000042")
	got = sweepSeeds(t, 7)
	if len(got) != 2 || got[1] != 1000042 {
		t.Fatalf("single seed: %v", got)
	}
	// Garbage fails loud (parser-level: sweepSeeds wraps it in Fatalf).
	if _, err := parseSeedEnv("bogus"); err == nil {
		t.Fatal("garbage accepted")
	}
	if _, err := parseSeedEnv("+bogus"); err == nil {
		t.Fatal("bad count accepted")
	}
	if _, err := parseSeedEnv("41,,42"); err == nil {
		t.Fatal("empty element accepted")
	}
}

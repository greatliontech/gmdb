//go:build dst

package dsttest

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
)

// extraSeedBase is where DST_SEEDS-count extras start: far above every
// pinned anchor so an extra can never collide with (or shadow) one,
// and derived deterministically so a failing extra seed is directly
// re-runnable and promotable to an anchor.
const extraSeedBase uint64 = 1_000_000

// sweepSeeds implements the suite seed policy (docs/specs/dst-testing.md
// §Seed policy): the PINNED anchor seeds always run, and the DST_SEEDS
// environment variable extends the sweep — "+N" appends N consecutive
// seeds from extraSeedBase, and a comma-separated list of seed values
// (including a single one) appends exactly those. The "+" prefix is the
// disambiguator: a bare number is always a seed VALUE, so a reported
// seed pastes verbatim. The extension is logged, so a sweep's coverage
// is always stated.
func sweepSeeds(t *testing.T, anchors ...uint64) []uint64 {
	t.Helper()
	seeds := append([]uint64(nil), anchors...)
	env := strings.TrimSpace(os.Getenv("DST_SEEDS"))
	if env == "" {
		return seeds
	}
	extras, err := parseSeedEnv(env)
	if err != nil {
		t.Fatalf("DST_SEEDS: %v", err)
	}
	seeds = append(seeds, extras...)
	t.Logf("DST_SEEDS=%s: sweeping %d extra seeds", env, len(extras))
	return seeds
}

// parseSeedEnv parses the DST_SEEDS grammar: "+N" = count form (N
// consecutive seeds from extraSeedBase), else a comma-separated list of
// explicit seed values. Pure so the grammar's error paths are testable.
func parseSeedEnv(env string) ([]uint64, error) {
	if rest, ok := strings.CutPrefix(env, "+"); ok {
		n, err := strconv.ParseUint(strings.TrimSpace(rest), 10, 32)
		if err != nil {
			return nil, fmt.Errorf("%q: count form is +N: %v", env, err)
		}
		out := make([]uint64, 0, n)
		for i := uint64(0); i < n; i++ {
			out = append(out, extraSeedBase+i)
		}
		return out, nil
	}
	var out []uint64
	for _, f := range strings.Split(env, ",") {
		v, err := strconv.ParseUint(strings.TrimSpace(f), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%q: neither +N count nor comma-separated seed list: %v", env, err)
		}
		out = append(out, v)
	}
	return out, nil
}

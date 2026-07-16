//go:build race

package gmdb

// raceEnabled reports whether the race detector instruments this
// build. Allocation-count assertions calibrate against it: race
// instrumentation allocates shadow state that inflates
// testing.AllocsPerRun far above the un-instrumented counts.
const raceEnabled = true

//go:build dst && race

package dsttest

// raceEnabled mirrors the build's -race state: the fork's DPOR
// dependency instrumentation (compiler access hooks, runtime sync
// hooks) exists only in dst-race builds, so the exploration legs pick
// their mode from this (docs/specs/dst-testing.md §Exploration tier).
const raceEnabled = true

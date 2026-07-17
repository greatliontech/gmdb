// Package dsttest holds gmdb's deterministic-simulation test suites
// (docs/specs/dst-testing.md). Every test file is build-tagged `dst`
// and runs only under the DST toolchain fork via the Taskfile
// `test:dst` leg (`godst test -tags dst ./dsttest/`); this untagged
// doc file keeps the package visible to ordinary builds, which
// otherwise contain no code here.
package dsttest

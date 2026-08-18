// Package flock is the platform seam for whole-file advisory
// locking on an open descriptor, serving two consumers: the
// database's cross-process write lock (cross-process.md §Write
// Lock) and the public oslock claim package (oslock.md). On the
// unix family every operation is flock(2) directly; on windows the
// semantics are emulated with a one-byte LockFileEx range at
// 2^63−1 — beyond any byte the lock file ever contains, because
// windows byte-range locks are mandatory against ReadFile/WriteFile.
// The conversion variant exists because flock documents
// shared→exclusive conversion as non-atomic (release then acquire) —
// on unix the kernel performs it in one call, on windows the seam
// performs the release explicitly — and every conversion caller
// tolerates the resulting race by design (contention ⇒ back off /
// retry, re-validating under the new lock). Contention and
// EINTR-class retry classification live behind the seam because the
// signals are platform-specific.
package flock

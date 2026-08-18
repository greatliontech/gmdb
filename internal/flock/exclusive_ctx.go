package flock

import (
	"context"
	"time"
)

// ExclusiveCtx acquires the exclusive lock by polling TryExclusive,
// honoring ctx. Polling — never a blocking flock — is the repo's
// settled answer to cancellable acquisition (cross-process.md §Write
// Lock): a cancelled wait leaves zero goroutines, zero descriptors,
// and zero abandoned kernel waiters behind. EINTR retries
// immediately without consuming a tick (the kernel made no
// contention decision); contention waits one interval per probe.
func ExclusiveCtx(ctx context.Context, fd uintptr, interval time.Duration) error {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		err := TryExclusive(fd)
		if err == nil {
			return nil
		}
		if ErrRetryable(err) {
			select {
			case <-ctx.Done():
				return context.Cause(ctx)
			default:
				continue
			}
		}
		if !ErrContended(err) {
			return err
		}
		select {
		case <-t.C:
			continue
		case <-ctx.Done():
			return context.Cause(ctx)
		}
	}
}

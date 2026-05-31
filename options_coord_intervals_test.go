package gmdb

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/thegrumpylion/gmdb/internal/lock"
)

// TestOptionsCoordIntervalsWiring proves the three cross-process
// coordination intervals flow from Options through CoordOptions into
// the live Coord — the CoordOptions plumbing for HeartbeatInterval /
// RetryInterval was previously always defaulted, and StaleTimeout had
// none. Custom non-default values must now reach the Coord verbatim.
func TestOptionsCoordIntervalsWiring(t *testing.T) {
	ctx := context.Background()
	const (
		customStale = 25 * time.Second
		customHB    = 2 * time.Second
		customRetry = 100 * time.Millisecond
	)
	db, err := Open(ctx, tmpPath(t), Options{
		PageSize:          4096,
		MinSize:           16,
		MaxSize:           128,
		StaleTimeout:      customStale,
		HeartbeatInterval: customHB,
		LockRetryInterval: customRetry,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	if got := db.coord.StaleTimeout(); got != customStale {
		t.Errorf("coord StaleTimeout = %v, want %v (Options.StaleTimeout not threaded)", got, customStale)
	}
	if got := db.coord.HeartbeatInterval(); got != customHB {
		t.Errorf("coord HeartbeatInterval = %v, want %v (Options.HeartbeatInterval not threaded)", got, customHB)
	}
	if got := db.coord.RetryInterval(); got != customRetry {
		t.Errorf("coord RetryInterval = %v, want %v (Options.LockRetryInterval not threaded)", got, customRetry)
	}
}

// TestOptionsCoordIntervalsDefault confirms the zero value still routes
// each interval to its lock-package default — applyDefaults and the
// Coord's own zero⇒default fallback must agree on a single source of
// truth.
func TestOptionsCoordIntervalsDefault(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	if got := db.coord.StaleTimeout(); got != lock.DefaultStaleTimeout {
		t.Errorf("default StaleTimeout = %v, want %v", got, lock.DefaultStaleTimeout)
	}
	if got := db.coord.HeartbeatInterval(); got != lock.DefaultHeartbeatInterval {
		t.Errorf("default HeartbeatInterval = %v, want %v", got, lock.DefaultHeartbeatInterval)
	}
	if got := db.coord.RetryInterval(); got != lock.DefaultRetryInterval {
		t.Errorf("default RetryInterval = %v, want %v", got, lock.DefaultRetryInterval)
	}
}

// TestOptionsCoordIntervalsValidation pins the data-integrity relation
// StaleTimeout > HeartbeatInterval (cross-process.md §Heartbeat
// Goroutine) and the non-negative bound. A window at or below the
// heartbeat cadence lets a jitter-delayed tick misclassify a live
// reader slot as stale, so Open must reject it rather than silently
// configure use-after-reclaim.
func TestOptionsCoordIntervalsValidation(t *testing.T) {
	ctx := context.Background()
	base := func() Options {
		return Options{PageSize: 4096, MinSize: 16, MaxSize: 128}
	}
	cases := []struct {
		name string
		mut  func(*Options)
		want error
	}{
		{
			name: "stale equals heartbeat",
			mut:  func(o *Options) { o.StaleTimeout = time.Second; o.HeartbeatInterval = time.Second },
			want: errStaleTimeoutTooSmall,
		},
		{
			name: "stale below heartbeat",
			mut:  func(o *Options) { o.StaleTimeout = 500 * time.Millisecond; o.HeartbeatInterval = time.Second },
			want: errStaleTimeoutTooSmall,
		},
		{
			// HeartbeatInterval zero ⇒ default 1s; an explicit
			// sub-second StaleTimeout must still be rejected against
			// the *effective* (defaulted) heartbeat cadence.
			name: "stale below default heartbeat",
			mut:  func(o *Options) { o.StaleTimeout = 500 * time.Millisecond },
			want: errStaleTimeoutTooSmall,
		},
		{
			name: "negative stale",
			mut:  func(o *Options) { o.StaleTimeout = -1 },
			want: errInvalidCoordInterval,
		},
		{
			name: "negative heartbeat",
			mut:  func(o *Options) { o.HeartbeatInterval = -1 },
			want: errInvalidCoordInterval,
		},
		{
			name: "negative lock-retry",
			mut:  func(o *Options) { o.LockRetryInterval = -1 },
			want: errInvalidCoordInterval,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o := base()
			tc.mut(&o)
			_, err := Open(ctx, tmpPath(t), o)
			if !errors.Is(err, ErrInvalidOptions) {
				t.Fatalf("got %v, want wrapped ErrInvalidOptions", err)
			}
			if !errors.Is(err, tc.want) {
				t.Errorf("got %v, want %v", err, tc.want)
			}
		})
	}
}

// TestOptionsCoordIntervalsValidConfig confirms a deliberately tuned
// (but safe) configuration — a large StaleTimeout/HeartbeatInterval
// pair plus a tiny retry — passes validation and opens.
func TestOptionsCoordIntervalsValidConfig(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{
		PageSize:          4096,
		MinSize:           16,
		MaxSize:           128,
		StaleTimeout:      30 * time.Second,
		HeartbeatInterval: 3 * time.Second,
		LockRetryInterval: time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Open with valid custom intervals: %v", err)
	}
	_ = db.Close()
}

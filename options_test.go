package gmdb

import (
	"testing"
	"time"

	"github.com/thegrumpylion/gmdb/internal/lock"
)

// TestCrossNamespaceStaleTimeoutValidation pins the cross-NS window's
// Options contract: zero defaults to 6 × StaleTimeout; an explicit
// value tighter than StaleTimeout is rejected (the window widens,
// never tightens — cross-process.md §Stale-reader detection).
func TestCrossNamespaceStaleTimeoutValidation(t *testing.T) {
	o := Options{}.applyDefaults()
	if o.CrossNamespaceStaleTimeout != 6*o.StaleTimeout {
		t.Errorf("default CrossNamespaceStaleTimeout = %v, want %v", o.CrossNamespaceStaleTimeout, 6*o.StaleTimeout)
	}
	bad := Options{CrossNamespaceStaleTimeout: 1 * time.Second}.applyDefaults()
	if err := bad.validate(); err == nil {
		t.Error("CrossNamespaceStaleTimeout < StaleTimeout accepted")
	}
	okOpt := Options{CrossNamespaceStaleTimeout: lock.DefaultStaleTimeout}.applyDefaults()
	if err := okOpt.validate(); err != nil {
		t.Errorf("CrossNamespaceStaleTimeout == StaleTimeout rejected: %v", err)
	}
}

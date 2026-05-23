//go:build linux

package lock

import (
	"os"
	"testing"
)

func TestProcessStartTimeSelf(t *testing.T) {
	ts, err := ProcessStartTime(os.Getpid())
	if err != nil {
		t.Fatalf("ProcessStartTime(self): %v", err)
	}
	if ts == 0 {
		t.Errorf("self start time = 0; want non-zero (clock ticks since boot)")
	}
	// Stable across repeated calls: the same process can't restart
	// mid-test.
	ts2, err := ProcessStartTime(os.Getpid())
	if err != nil {
		t.Fatalf("second ProcessStartTime(self): %v", err)
	}
	if ts2 != ts {
		t.Errorf("self start time changed: %d → %d", ts, ts2)
	}
}

func TestProcessStartTimeDeadPID(t *testing.T) {
	// Same impossibly-high PID as in TestIsAliveImpossiblePID; the
	// /proc/<pid>/stat file does not exist so ReadFile surfaces an
	// ErrNotExist-wrapped error.
	_, err := ProcessStartTime(0x7FFFFFFF)
	if err == nil {
		t.Errorf("ProcessStartTime(impossibly-high) = nil; want error")
	}
}

func TestParseStartTime(t *testing.T) {
	// Canonical /proc/<pid>/stat shape: real format observed on
	// Linux 6.x. Field 22 (1-based) here is 12345.
	stat := `1234 (kworker/u8:0) S 2 0 0 0 -1 69238880 0 0 0 0 0 0 0 0 20 0 1 0 12345 0 0 4 0 0 0 0 0 0 0 0 0 0 0 0 0 0 17 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0` + "\n"
	got, err := parseStartTime(stat)
	if err != nil {
		t.Fatalf("parseStartTime: %v", err)
	}
	if got != 12345 {
		t.Errorf("parseStartTime = %d, want 12345", got)
	}
}

func TestParseStartTimeCommWithParens(t *testing.T) {
	// Adversarial comm containing ')' characters. The canonical fix
	// is to split on the LAST ')'; this test pins that behavior.
	stat := `4242 (bad ) name (with parens)) S 1 1 1 0 -1 4194560 100 0 0 0 0 0 0 0 20 0 1 0 99999 0 0 4 0 0 0 0 0 0 0 0 0 0 0 0 0 0 17 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0` + "\n"
	got, err := parseStartTime(stat)
	if err != nil {
		t.Fatalf("parseStartTime: %v", err)
	}
	if got != 99999 {
		t.Errorf("parseStartTime with adversarial comm = %d, want 99999", got)
	}
}

func TestParseStartTimeMalformed(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"no_rparen", "1234 (foo S 1"},
		{"too_few_fields", "1234 (foo) S 1 2 3"},
		{"non_numeric_starttime", `1234 (foo) S 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 abc 0`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := parseStartTime(c.in); err == nil {
				t.Errorf("expected error, got nil")
			}
		})
	}
}

func TestPIDNamespaceSelf(t *testing.T) {
	ns, err := PIDNamespace()
	if err != nil {
		// In a hardened-sandbox CI environment /proc/self/ns/pid may
		// be unavailable. The contract is that the error surfaces so
		// the caller can log it; subsequent stale-detection uses the
		// 0 return value and routes through the heartbeat fallback.
		t.Logf("PIDNamespace returned error (likely no /proc): %v", err)
		if ns != 0 {
			t.Errorf("PIDNamespace returned ns=%d on error, want 0", ns)
		}
		return
	}
	if ns == 0 {
		t.Errorf("PIDNamespace returned (0, nil); want non-zero inode or an error")
	}
	// Stable across repeated calls.
	ns2, err := PIDNamespace()
	if err != nil {
		t.Fatalf("second PIDNamespace: %v", err)
	}
	if ns2 != ns {
		t.Errorf("PIDNamespace changed: %d → %d", ns, ns2)
	}
}

func TestParseNSLink(t *testing.T) {
	cases := []struct {
		in     string
		want   uint64
		wantOK bool
	}{
		{"pid:[4026531836]", 4026531836, true},
		{"pid:[0]", 0, true}, // pin: zero inode is parse-valid; the higher-level "0 means no namespace" convention is in the caller
		{"pid:[]", 0, false},
		{"pid:4026531836", 0, false}, // no brackets
		{"", 0, false},
		{"pid:[abc]", 0, false},
	}
	for _, c := range cases {
		got, err := parseNSLink(c.in)
		gotOK := err == nil
		if gotOK != c.wantOK {
			t.Errorf("parseNSLink(%q) ok = %v, want %v (err=%v)", c.in, gotOK, c.wantOK, err)
			continue
		}
		if gotOK && got != c.want {
			t.Errorf("parseNSLink(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

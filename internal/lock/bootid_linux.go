//go:build linux

package lock

import (
	"encoding/hex"
	"os"
	"sync"
)

// CurrentBootID returns the running kernel's boot UUID
// (/proc/sys/kernel/random/boot_id) as raw bytes — the boot-epoch
// discriminator stamped into the lock-file header. Read once per
// process (the value is constant for a boot, including across
// suspend/resume). A read or parse failure yields the zero value;
// callers treat a zero BootID as "epoch unknown": cross-boot
// invalidation is DISABLED whenever either side is unknown (see
// shouldResetBootEpoch) — resetting on an unknown epoch could evict
// LIVE same-boot peers' coordination state, which is strictly worse
// than the pre-boot-epoch hazard the reset exists to fix.
func CurrentBootID() [16]byte {
	bootIDOnce.Do(func() { bootIDValue = readBootID() })
	return bootIDValue
}

var (
	bootIDOnce  sync.Once
	bootIDValue [16]byte
)

func readBootID() [16]byte {
	var id [16]byte
	raw, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return id
	}
	// "xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx\n" → 16 bytes.
	compact := make([]byte, 0, 32)
	for _, c := range raw {
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f', c >= 'A' && c <= 'F':
			compact = append(compact, c)
		}
	}
	if len(compact) != 32 {
		return [16]byte{}
	}
	if _, err := hex.Decode(id[:], compact); err != nil {
		return [16]byte{}
	}
	return id
}

// shouldResetBootEpoch reports whether an adoption must run the
// cross-boot reset: ONLY when both the stamped and the current boot
// id are KNOWN (non-zero) and differ. A zero on either side means an
// epoch could not be learned (unreadable /proc in a chroot or mount
// namespace, or a header stamped by such a process) — invalidation is
// then disabled: a reset justified by an unknown epoch could zero a
// LIVE same-boot peer's slots (its reads then run unpinned —
// use-after-reclaim), which is strictly worse than the cross-boot
// staleness the reset exists to fix. In zero-epoch environments the
// pre-boot-epoch semantics (future-stamp guard only) apply, and the
// spec documents that residual.
func shouldResetBootEpoch(stamped, current [16]byte) bool {
	var zero [16]byte
	if stamped == zero || current == zero {
		return false
	}
	return stamped != current
}

//go:build !linux

package lock

// CurrentBootID returns the zero boot id on platforms without a boot
// discriminator source; shouldResetBootEpoch never fires on a zero,
// so cross-boot invalidation is disabled — the designed degradation
// for non-Linux platforms (cross-process.md §Lock File Layout).
func CurrentBootID() [16]byte { return [16]byte{} }

// shouldResetBootEpoch — see bootid_linux.go. Fires only when both
// ids are known and differ.
func shouldResetBootEpoch(stamped, current [16]byte) bool {
	var zero [16]byte
	if stamped == zero || current == zero {
		return false
	}
	return stamped != current
}

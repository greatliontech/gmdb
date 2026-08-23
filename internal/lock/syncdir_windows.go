//go:build windows

package lock

import "os"

// syncDir is a no-op on windows: FlushFileBuffers on a directory
// handle is refused (Access is denied) through os.File.Sync, and
// NTFS journals directory metadata on its own schedule. The
// power-loss window this leaves — a durable lock-file header whose
// readers-table dirents were lost — self-heals at the next
// cross-boot adoption: the boot-epoch reset repopulates a missing
// table under its LOCK_EX (no holder from the dead boot can exist),
// so the fail-closed open path wedges at worst until that reset
// runs (cross-process.md §Reader Table, slot locks, windows arm).
func syncDir(root *os.Root, name string) error { return nil }

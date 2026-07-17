//go:build !linux

package gmdb

// renameNoReplace — off Linux the probe+rename best-effort form IS
// the publish (api-surface.md §Check, CopyTo, Compact per-filesystem
// contract). Some platforms do have an atomic form (darwin's
// renameatx_np(RENAME_EXCL)); the contract deliberately pins
// best-effort for every non-Linux platform rather than growing a
// per-OS syscall matrix the project cannot exercise in CI.
func renameNoReplace(tmp, path string) error {
	return renameNoReplaceBestEffort(tmp, path)
}

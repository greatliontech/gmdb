//go:build !windows

package lock

import (
	"fmt"
	"os"
)

// syncDir fsyncs a directory within root, pinning its dirents — the
// durable-table half of populateReadersDir's ordering contract.
func syncDir(root *os.Root, name string) error {
	d, err := root.Open(name)
	if err != nil {
		return fmt.Errorf("lock: open dir %q for fsync: %w", name, err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("lock: fsync dir %q: %w", name, err)
	}
	return nil
}

package indexing

import (
	"errors"
	"strings"
	"testing"
)

// TestRegistryEntryRejectsOversizedUserVersion verifies the encoder
// rejects a UserVersion exceeding uint16. Necessary because the
// on-disk format uses uint16 length prefixes — silent truncation
// would corrupt the encoded entry.
func TestRegistryEntryRejectsOversizedUserVersion(t *testing.T) {
	e := &RegistryEntry{UserVersion: strings.Repeat("x", 70000)}
	_, err := EncodeRegistryEntry(e)
	if !errors.Is(err, ErrFieldTooLarge) {
		t.Fatalf("expected ErrFieldTooLarge for oversized UserVersion, got %v", err)
	}
}

// TestRegistryEntryRejectsOversizedColumnName verifies the encoder
// rejects a column name exceeding uint16.
func TestRegistryEntryRejectsOversizedColumnName(t *testing.T) {
	e := &RegistryEntry{Columns: []string{strings.Repeat("x", 70000)}}
	_, err := EncodeRegistryEntry(e)
	if !errors.Is(err, ErrFieldTooLarge) {
		t.Fatalf("expected ErrFieldTooLarge for oversized column name, got %v", err)
	}
}

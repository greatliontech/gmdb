package lock

import (
	"structs"
	"unsafe"
)

// Magic identifies a gmdb lock file. Encoded little-endian on disk so
// a hex dump reads "gmdblock" left-to-right on x86/arm64.
const Magic uint64 = 0x6B636F6C62646D67

// HeaderSize is the on-disk byte length of LockFileHeader. Frozen at
// the value enforced by the compile-time size check in this file.
const HeaderSize = 80

// SlotSize is the on-disk byte length of one ReaderSlot, ditto.
const SlotSize = 48

// MaxReaders bounds. The default is the api-surface.md
// Options.MaxReaders default (4096); the floor is a sanity minimum
// that still leaves room for a few concurrent readers in tests; the
// ceiling caps the lock-file size to a comfortable ~3 MiB at 65536
// slots so a malformed/malicious MaxReaders cannot demand a
// petabyte-sized mmap.
const (
	MinMaxReaders     uint32 = 1
	MaxMaxReaders     uint32 = 65536
	DefaultMaxReaders uint32 = 4096
)

// LockFileHeader overlays the first HeaderSize bytes of the lock file
// mmap. structs.HostLayout disables Go's freedom to reorder fields,
// pinning the on-disk byte offsets to the natural C ABI layout this
// spec documents.
//
// Every uint64 field is 8-byte aligned by construction: the mmap base
// is page-aligned (MAP_SHARED ⇒ ≥ 4096-byte alignment), Magic at
// offset 0 is 8-aligned, MaxReaders+padding consume bytes 8..15, UUID
// is byte-aligned (and self-contained), and the trailing uint64s start
// at offset 32 — all multiples of 8.
type LockFileHeader struct {
	_                   structs.HostLayout
	Magic               uint64
	MaxReaders          uint32
	_                   [4]byte
	UUID                [16]byte
	WriterPID           uint64
	WriterStartTime     uint64
	WriterPIDNamespace  uint64
	WriterHeartbeat     uint64
	LastMaintenanceTime uint64
	// DataGeneration counts data-file replacements (Compact's
	// rename-over). A handle caches the value at Open and re-checks it
	// after every write-grant acquisition and reader-slot publish: a
	// mismatch means a peer replaced the inode this handle still maps —
	// continuing would commit to (or read) the unlinked file, silently
	// diverging from every other process. Bumped atomically by Compact
	// under the write grant, after the rename + directory fsync.
	DataGeneration uint64
}

// ReaderSlot overlays one 48-byte slot in the reader table. All six
// fields are atomically accessed via the helpers in atomic.go;
// cross-process visibility of these fields is the entire purpose of
// the lock-file design.
type ReaderSlot struct {
	_                structs.HostLayout
	TxnID            uint64
	PID              uint64
	ProcessStartTime uint64
	PIDNamespace     uint64
	Heartbeat        uint64
	HintEpoch        uint64
}

// Compile-time assertions that the structs' Go sizes match the spec's
// on-disk sizes. structs.HostLayout makes this stable across the
// supported architectures (amd64, arm64) because both have the same
// C ABI for these primitive sizes; if a future port lands on a
// platform with a different ABI, these will fail to compile and the
// porter must revisit the layout.
var (
	_ [HeaderSize]byte = [unsafe.Sizeof(LockFileHeader{})]byte{}
	_ [SlotSize]byte   = [unsafe.Sizeof(ReaderSlot{})]byte{}
)

// FileSize returns the total mmap size for a lock file with
// maxReaders slots: HeaderSize + SlotSize × maxReaders. The
// cross-process.md mmap-size invariant requires every process to
// mmap exactly this many bytes.
func FileSize(maxReaders uint32) int64 {
	return int64(HeaderSize) + int64(SlotSize)*int64(maxReaders)
}

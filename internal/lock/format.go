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
const HeaderSize = 136

// SlotSize is the on-disk byte length of one ReaderSlot, ditto.
const SlotSize = 56

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
// offset 0 is 8-aligned, MaxReaders+TakeoverSeq consume bytes 8..15
// (TakeoverSeq at offset 12 is 4-aligned, as 32-bit atomics require), UUID
// is byte-aligned (and self-contained), and the trailing uint64s start
// at offset 32 — all multiples of 8.
type LockFileHeader struct {
	_          structs.HostLayout
	Magic      uint64
	MaxReaders uint32
	// TakeoverSeq counts torn-unpublished-write events — the two
	// states in which torn reclamation writes can exist unpublished
	// (cross-process.md §Lock File Layout, takeover sequence): a
	// grant acquisition observing a non-zero WriterPID (a holder
	// that died without its clear-before-unlock) bumps under its
	// LOCK_EX with NO liveness classifier (a false-live window would
	// swallow the bump permanently); a publication-phase commit
	// failure bumps from the poisoning author itself, under the
	// grant it still holds (a clean release leaves no WriterPID
	// evidence). Each handle caches the value its
	// in-memory bitmap + RPL chain was last (re)built at and forces a
	// full rebuild from the on-disk image when they differ.
	// Level-triggered on purpose: the crashed holder's header is
	// consumed by the FIRST acquisition (recovery clears it, the
	// stamp overwrites it), while its torn, never-published
	// reclamation poisons EVERY surviving handle's chain without
	// advancing TxnID (free-space.md §Grant-handoff tear detection).
	// Occupies what was header
	// padding, so HeaderSize is unchanged (see the size-growth safety
	// invariant at the adopt-time size check); pre-takeover lock files
	// carry 0. uint32 wrap would need 2^32 dead-writer takeovers
	// between two grants of one handle.
	TakeoverSeq         uint32
	UUID                [16]byte
	WriterPID           uint64
	WriterStartTime     uint64
	WriterPIDNamespace  uint64
	WriterHeartbeat     uint64
	LastMaintenanceTime uint64
	// LastWriter* persist the identity of the most recent write-grant
	// holder ACROSS grant release (the Writer* block above is zeroed on
	// release). Only the last writer's process can hold unfsynced live
	// commits in the shared page cache, so its liveness is the signal
	// the recovery-commit gate needs to distinguish a crashed database
	// from a live one with an idle author (durability.md §Recovery
	// step 5). Written at every grant acquisition; LastWriterHeartbeat
	// is refreshed by the author handle's heartbeat goroutine for the
	// handle's LIFETIME (not just while the grant is held) and goes
	// stale when the process dies — same classification rules as
	// reader slots (cross-process.md §Reader Table).
	LastWriterPID          uint64
	LastWriterStartTime    uint64
	LastWriterPIDNamespace uint64
	LastWriterHeartbeat    uint64
	// DataGeneration counts data-file replacements (Compact's
	// rename-over). A handle caches the value at Open and re-checks it
	// after every write-grant acquisition and reader-slot publish: a
	// mismatch means a peer replaced the inode this handle still maps —
	// continuing would commit to (or read) the unlinked file, silently
	// diverging from every other process. Bumped atomically by Compact
	// under the write grant, after the rename + directory fsync.
	// SURVIVES the boot-epoch reset (it counts inode replacements,
	// not boot-relative time).
	DataGeneration uint64
	// BootID is the boot-epoch discriminator (Linux
	// /proc/sys/kernel/random/boot_id): every heartbeat and process
	// start time in this file is meaningful ONLY within the boot that
	// stamped it (CLOCK_BOOTTIME and starttime ticks are boot-relative,
	// and PID/starttime identities can collide across boots). An
	// adopter whose current boot differs — BOTH ids known (non-zero);
	// see shouldResetBootEpoch — resets the volatile coordination
	// state — writer blocks, every reader slot — under flock(LOCK_EX)
	// and stamps the current boot: with both epochs known, no process
	// from the stamped boot can still exist, so nothing live is
	// evicted (cross-process.md §Lock File Layout, boot epoch).
	BootID [16]byte
	// ShrinkSeq is the file-shrink seqlock (file-format.md §File
	// Shrinkage): the writer increments it to ODD before the
	// reader-visibility scan that precedes an ftruncate and to EVEN
	// after the truncate lands; a reader brackets its size read
	// (slot publish → read seq → fstat → re-read seq; odd or changed
	// ⇒ re-fstat). Closes the reader-CAS acquisition window during
	// which a freshly-published reader could retain a pre-shrink
	// file-resident bound.
	ShrinkSeq uint64
}

// ReaderSlot overlays one 56-byte slot in the reader table. All seven
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
	// Gen is the slot's acquisition generation: monotonically bumped
	// by every successful TxnID CAS, never zeroed by release or
	// clear. An owner's ownership token is (TxnID, Gen-at-acquire):
	// the post-publish verify, the release, and the restabilization
	// raise all re-check it, so a slot lost to an aging clear and
	// re-won — even at the SAME pinned TxnID — is never mistaken for
	// still-owned (cross-process.md §Slot acquire).
	Gen uint64
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

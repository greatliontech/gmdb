package lock

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

// Errors surfaced by the lock-file lifecycle. Sentinels so callers
// can branch via errors.Is.
var (
	// ErrCorrupted is returned when an existing lock file fails
	// validation (Magic mismatch, MaxReaders out of range, file size
	// inconsistent with the header). The chunk-2 lifecycle treats
	// these as recoverable by delete-and-recreate when the cause is
	// a stale leftover; a freshly-created lock file that fails its
	// own validation indicates a runtime defect and surfaces this
	// error to the caller. The root package's mapping translates
	// to gmdb.ErrCorrupted.
	ErrCorrupted = errors.New("lock: lock file structurally invalid")

	// ErrInvalidMaxReaders is returned by Open when the caller's
	// requested MaxReaders falls outside [MinMaxReaders, MaxMaxReaders].
	ErrInvalidMaxReaders = errors.New("lock: MaxReaders out of range")

	// ErrInvalidBase is returned by Open when the Base name contains
	// path separators or other characters incompatible with an
	// os.Root-confined open.
	ErrInvalidBase = errors.New("lock: Base must be a simple file name")

	// errStaleUUID is an internal sentinel signalling that the lock
	// file exists but belongs to a different database (UUID mismatch).
	// The lifecycle loop unlinks and retries; not exposed to callers.
	errStaleUUID = errors.New("lock: existing file has UUID mismatch (stale)")

	// errPartialInit is an internal sentinel signalling that the lock
	// file exists but is mid-init (size < HeaderSize, or size correct
	// but header Magic still zero). A concurrent creator is in the
	// O_CREATE|O_EXCL → flock(LOCK_EX) window — that gap is
	// unavoidable on POSIX (open and flock are separate syscalls),
	// so adopters must retry on this signal rather than treat it as
	// terminal corruption. Not exposed to callers; the lifecycle
	// loop translates it into a backoff-and-retry.
	errPartialInit = errors.New("lock: file mid-init; creator has not yet acquired LOCK_EX")
)

// OpenParams configures the lock file's discovery + creation.
//
// The lock file is identified by a directory (Root) + a base name
// (Base) rather than an absolute path. This composes with chunk-1's
// path-traversal guard: db.Open opens the data file's directory via
// os.OpenRoot, and the same *os.Root is passed here so the lock file
// shares the same symlink-escape protection.
type OpenParams struct {
	// Root is an os.Root confining all path operations to a single
	// directory. db.Open's existing Root for the data file's
	// directory is the canonical caller-supplied value. Required.
	Root *os.Root

	// Base is the lock file's name within Root. Conventionally
	// "<datafile>.lock"; the BaseFor helper produces this. Must
	// contain no path separators (validated against ErrInvalidBase).
	Base string

	// DataUUID is the data file's meta-page UUID. Used to validate
	// an existing lock file (mismatch ⇒ stale ⇒ delete-and-recreate)
	// and stamped into a freshly-created header.
	DataUUID [16]byte

	// MaxReaders is the requested reader-table capacity for a newly
	// created lock file. Ignored when an existing lock file is
	// adopted — its header value is authoritative. Must be in
	// [MinMaxReaders, MaxMaxReaders].
	MaxReaders uint32
}

// File is an opened lock file. Holds the on-disk file descriptor (so
// the flock goroutine can invoke flock() on it in chunk 2.4) plus the
// MAP_SHARED mmap region overlaid with typed accessors for the header
// and each reader slot.
//
// Lifetime contract. After Close, every accessor on *File becomes
// invalid:
//   - The header / slot pointer accessors (Slot, the typed header
//     getters/setters) panic on nil-deref because the overlay pointers
//     are cleared. Calling them after Close is a programmer bug.
//   - A *ReaderSlot pointer returned by Slot before Close references
//     mmap memory that is unmapped by Close — any atomic op against
//     that pointer SIGSEGVs and brings the process down. Callers must
//     either (a) drop slot pointers before calling Close, or (b)
//     ensure no concurrent goroutine holds a slot pointer at Close
//     time. The heartbeat goroutine's slot-pointer ownership is the
//     concrete instance of (b) — see leak-detection.md §Close
//     Ordering and cross-process.md §Heartbeat Goroutine.
type File struct {
	f      *os.File
	mmap   []byte
	header *LockFileHeader
	slots  []ReaderSlot
}

// Open returns a *File for the lock file at Base within Root. The
// lifecycle is symlink-safe (all path operations confined by
// os.Root) and race-safe across three windows:
//
//   - Create vs adopt race: the O_CREATE|O_EXCL syscall elects a
//     single creator; the loser sees EEXIST and falls into the
//     adoption path.
//   - Creator's open→flock window: between O_CREATE|O_EXCL and the
//     creator's first flock(LOCK_EX), the file is visible on disk
//     with size 0 and no holder of LOCK_EX. POSIX provides no
//     atomic open-and-flock, so this gap is unavoidable. An adopter
//     that lands inside it returns errPartialInit, the lifecycle
//     retries with backoff until the creator publishes (or the
//     budget exhausts).
//   - Init publication: while the creator holds LOCK_EX through
//     Truncate + WriteAt + Sync, adopters take LOCK_SH which blocks
//     until LOCK_EX is released. By the time LOCK_SH is granted,
//     the file's header is fully published.
//
// UUID mismatch unlinks the stale file and retries; a Magic /
// MaxReaders / size validation failure on a finalised file surfaces
// ErrCorrupted without unlinking (chunk-2 does not auto-recover
// externally-tampered or crashed-mid-init files).
//
// Retry budget. Adopter sees errPartialInit at most O(init-window)
// times before the creator publishes. Init is bounded by one
// Truncate + 72-byte WriteAt + one fdatasync — typically sub-ms on
// SSD, up to ~100 ms on contended HDDs. The budget + capped
// exponential backoff (1, 2, 4, 8, 16, 32, 64, 128, 256, 256 ms;
// total ~800 ms) covers a slow disk while bounding caller latency.
func Open(p OpenParams) (*File, error) {
	if p.Root == nil {
		return nil, fmt.Errorf("lock: Root must not be nil")
	}
	if strings.ContainsAny(p.Base, "/\x00") {
		return nil, fmt.Errorf("lock: Base %q: %w", p.Base, ErrInvalidBase)
	}
	if p.MaxReaders < MinMaxReaders || p.MaxReaders > MaxMaxReaders {
		return nil, fmt.Errorf("lock: requested MaxReaders %d outside [%d, %d]: %w",
			p.MaxReaders, MinMaxReaders, MaxMaxReaders, ErrInvalidMaxReaders)
	}

	const maxAttempts = 10
	const maxBackoff = 256 * time.Millisecond
	backoff := time.Millisecond
	var lastErr error
	for range maxAttempts {
		f, err := tryAdoptExisting(p)
		if err == nil {
			return f, nil
		}
		if errors.Is(err, errStaleUUID) {
			if rmErr := p.Root.Remove(p.Base); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
				return nil, fmt.Errorf("lock: remove stale %q: %w", p.Base, rmErr)
			}
			lastErr = err
			continue
		}
		if errors.Is(err, errPartialInit) {
			// Creator is in the open→flock(LOCK_EX) window or holds
			// LOCK_EX but our LOCK_SH was released before the size
			// check could observe the full publication. Back off and
			// retry adoption; do NOT fall through to createAndInit,
			// because the file does exist on disk and a fresh
			// O_CREATE|O_EXCL would just spin EEXIST.
			lastErr = err
			time.Sleep(backoff)
			if backoff < maxBackoff {
				backoff *= 2
				if backoff > maxBackoff {
					backoff = maxBackoff
				}
			}
			continue
		}
		if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}

		f, err = createAndInit(p)
		if err == nil {
			return f, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		// Lost the O_CREATE|O_EXCL race. Loop to adopt the winner.
		lastErr = err
	}
	// Exhausted retries. If the terminal error was the partial-init
	// signal, no live creator advanced the file across the ~800 ms
	// budget — that's far longer than a legitimate
	// Truncate+WriteAt+Sync on any supported filesystem, so the file
	// is stuck (creator crashed between O_CREATE|O_EXCL and Flock,
	// or external tampering left a zero-Magic file at this path).
	// Surface as ErrCorrupted so callers can use the standard
	// corruption-recovery surface.
	if errors.Is(lastErr, errPartialInit) {
		return nil, fmt.Errorf("lock: file at %q remained partially-initialised after %d attempts: %w",
			p.Base, maxAttempts, ErrCorrupted)
	}
	return nil, fmt.Errorf("lock: open lifecycle did not converge after %d attempts: %w",
		maxAttempts, lastErr)
}

// tryAdoptExisting opens Base via Root and validates the header. The
// adopter takes flock(LOCK_SH) immediately after open and before
// reading the header — this blocks until a concurrent creator
// releases LOCK_EX, guaranteeing the adopter only ever sees a
// (mostly) fully-initialised file.
//
// Partial-state handling. LOCK_SH does NOT serialise against the
// pre-LOCK_EX window of a concurrent creator (between
// O_CREATE|O_EXCL and the creator's first syscall.Flock). An
// adopter that lands inside this window observes size==0 and/or
// Magic==0; rather than returning terminal ErrCorrupted, it
// surfaces errPartialInit so the lifecycle loop in Open retries
// with backoff. Genuinely corrupt files (size != FileSize, Magic
// non-zero but wrong, MaxReaders out of range) still surface
// ErrCorrupted without retry.
//
// Returns os.ErrNotExist (propagated from Root.OpenFile) when the
// file doesn't exist; errStaleUUID when the header verifies but the
// UUID doesn't match p.DataUUID; errPartialInit on the creator
// publish window; ErrCorrupted (wrapped) for true Magic / MaxReaders
// / size mismatches.
func tryAdoptExisting(p OpenParams) (*File, error) {
	f, err := p.Root.OpenFile(p.Base, os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	// Defer-LIFO close+unlock pattern. Defers fire at function exit
	// in reverse registration order:
	//   1. (registered first) closeOnExit-guarded Close — frees the
	//      FD.
	//   2. (registered second, runs first) LOCK_UN — releases the
	//      flock while the FD is still alive.
	// Without this ordering, an explicit Close + later deferred
	// LOCK_UN would race a concurrent goroutine reusing the freed
	// FD number: the deferred Flock(fd, LOCK_UN) could land on the
	// other goroutine's fresh adopter LOCK_SH and silently release
	// it, letting that adopter proceed without serialisation
	// against the creator. (Race window: between close(fd) syscall
	// return and os/File's internal Sysfd=-1 assignment.) The
	// closeOnExit flag carries the success path through to the
	// returned *File which then owns the fd.
	closeOnExit := true
	defer func() {
		if closeOnExit {
			_ = f.Close()
		}
	}()

	// LOCK_SH blocks while any process holds LOCK_EX on this fd —
	// the legitimate holder is a concurrent creator running
	// initLockFile. Once we acquire LOCK_SH, the file is fully
	// published *if* the creator already reached its LOCK_EX call.
	// If the creator is still in the open→flock window, our LOCK_SH
	// succeeds immediately and we fall into the size==0 / Magic==0
	// detection below.
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH); err != nil {
		return nil, fmt.Errorf("lock: flock(LOCK_SH) on %q: %w", p.Base, err)
	}
	// Drop LOCK_SH at function exit — we don't need it for steady-
	// state use (the mmap is independent of flock). The writer's
	// later LOCK_EX (taken by the flock goroutine in 2.4) doesn't
	// conflict with our subsequent atomic mmap ops.
	defer func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }()

	st, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("lock: stat %q: %w", p.Base, err)
	}
	if st.Size() < int64(HeaderSize) {
		// Mid-init: the creator has O_CREATE|O_EXCL'd the file but
		// hasn't yet completed Truncate. Retryable.
		return nil, errPartialInit
	}

	headerBytes := make([]byte, HeaderSize)
	if _, err := f.ReadAt(headerBytes, 0); err != nil {
		return nil, fmt.Errorf("lock: read header %q: %w", p.Base, err)
	}
	hdr := (*LockFileHeader)(unsafe.Pointer(&headerBytes[0]))
	if hdr.Magic == 0 {
		// Mid-init: Truncate completed (so size matches a real
		// MaxReaders) but WriteAt hasn't landed yet. Retryable.
		// We use Magic == 0 specifically — Truncate zero-fills, so
		// the post-Truncate / pre-WriteAt state is exactly all-zero
		// header. A non-zero-but-wrong Magic falls through to the
		// ErrCorrupted branch below.
		return nil, errPartialInit
	}
	if hdr.Magic != Magic {
		return nil, fmt.Errorf("lock: %q magic 0x%016x != lock.Magic: %w",
			p.Base, hdr.Magic, ErrCorrupted)
	}
	if hdr.MaxReaders < MinMaxReaders || hdr.MaxReaders > MaxMaxReaders {
		return nil, fmt.Errorf("lock: %q MaxReaders %d outside [%d, %d]: %w",
			p.Base, hdr.MaxReaders, MinMaxReaders, MaxMaxReaders, ErrCorrupted)
	}
	expectedSize := FileSize(hdr.MaxReaders)
	if st.Size() != expectedSize {
		return nil, fmt.Errorf("lock: %q size %d != expected %d (MaxReaders=%d): %w",
			p.Base, st.Size(), expectedSize, hdr.MaxReaders, ErrCorrupted)
	}
	if hdr.UUID != p.DataUUID {
		return nil, errStaleUUID
	}

	// Success: the *File takes ownership of the fd; do not close it
	// on exit. The deferred LOCK_UN still runs (correct — the *File
	// doesn't need flock for steady-state use).
	out, err := mmapAndOverlay(f, hdr.MaxReaders, expectedSize)
	if err != nil {
		return nil, err
	}
	closeOnExit = false
	return out, nil
}

// createAndInit creates Base via Root.OpenFile with O_CREATE|O_EXCL,
// takes flock(LOCK_EX) immediately, and inits the file under the
// lock so adopters in any process taking LOCK_SH block until init
// completes.
//
// Returns os.ErrExist when another process won the O_CREATE|O_EXCL
// race; the caller loops to adoption.
//
// Uses the same closeOnExit defer pattern as tryAdoptExisting — see
// the comment there for the FD-reuse race motivation.
func createAndInit(p OpenParams) (*File, error) {
	f, err := p.Root.OpenFile(p.Base, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	closeOnExit := true
	removeOnExit := true
	defer func() {
		if closeOnExit {
			_ = f.Close()
		}
		if removeOnExit {
			// On any failure after O_CREATE|O_EXCL succeeded, unlink
			// the half-baked file so the next opener doesn't
			// repeatedly spin against a stuck Magic==0 / undersized
			// file (the budget-exhaustion path eventually surfaces
			// ErrCorrupted, but spinning ~800 ms per Open is a
			// real latency cost).
			_ = p.Root.Remove(p.Base)
		}
	}()

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return nil, fmt.Errorf("lock: flock(LOCK_EX) on freshly-created %q: %w", p.Base, err)
	}
	// LOCK_EX is held across init; release after init regardless of
	// outcome so adopters can take LOCK_SH and validate.
	initErr := initLockFile(f, p.DataUUID, p.MaxReaders, FileSize(p.MaxReaders))
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	if initErr != nil {
		return nil, initErr
	}

	out, err := mmapAndOverlay(f, p.MaxReaders, FileSize(p.MaxReaders))
	if err != nil {
		return nil, err
	}
	closeOnExit = false
	removeOnExit = false
	return out, nil
}

// initLockFile truncates the file to the full lock-file size and
// writes the header. Reader-table region is left zero — Truncate
// produces zero-filled holes on every supported filesystem, which is
// the "all slots free" initial state.
//
// Race semantics. The caller (createAndInit) holds flock(LOCK_EX)
// across this entire function plus Sync, so concurrent adopters
// blocked on LOCK_SH cannot observe the file's partial state. If
// the process crashes mid-init (after Truncate but before Sync),
// the kernel releases LOCK_EX on fd close; the next opener acquires
// LOCK_SH, reads the half-init'd header (Magic == 0), and surfaces
// ErrCorrupted. Manual cleanup is required — chunk-2 does not
// auto-recover from crashed-mid-init files.
func initLockFile(f *os.File, uuid [16]byte, maxReaders uint32, fileSize int64) error {
	if err := f.Truncate(fileSize); err != nil {
		return fmt.Errorf("lock: truncate: %w", err)
	}
	hdr := LockFileHeader{
		Magic:      Magic,
		MaxReaders: maxReaders,
		UUID:       uuid,
		// Writer header fields are zero on creation — no writer holds
		// the lock yet, so PID/StartTime/PIDNamespace/Heartbeat all
		// default to 0 per the convention in cross-process.md.
	}
	headerBytes := (*[HeaderSize]byte)(unsafe.Pointer(&hdr))[:]
	if _, err := f.WriteAt(headerBytes, 0); err != nil {
		return fmt.Errorf("lock: write header: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("lock: fsync init: %w", err)
	}
	return nil
}

// mmapAndOverlay mmaps the lock file and constructs a *File with the
// header / slots overlaid on the mapping. fileSize must match
// FileSize(maxReaders) — verified by the caller.
//
// On error, the caller retains ownership of f and must close it (the
// closeOnExit defer pattern in tryAdoptExisting / createAndInit). On
// success, ownership transfers to the returned *File.
func mmapAndOverlay(f *os.File, maxReaders uint32, fileSize int64) (*File, error) {
	mapping, err := mmapRW(f.Fd(), fileSize)
	if err != nil {
		return nil, fmt.Errorf("lock: mmap: %w", err)
	}
	if int64(len(mapping)) != fileSize {
		// Defensive: mmapRW should always return exactly fileSize
		// bytes; if some platform shim disagrees, surface rather
		// than risk a wrong-sized struct overlay.
		_ = munmap(mapping)
		return nil, fmt.Errorf("lock: mmap size %d != requested %d", len(mapping), fileSize)
	}
	header := (*LockFileHeader)(unsafe.Pointer(&mapping[0]))
	slots := unsafe.Slice(
		(*ReaderSlot)(unsafe.Pointer(&mapping[HeaderSize])),
		int(maxReaders),
	)
	return &File{
		f:      f,
		mmap:   mapping,
		header: header,
		slots:  slots,
	}, nil
}

// Close releases the mmap and closes the file descriptor. Idempotent.
// Does NOT unlink the lock file — it is ephemeral and removal is
// orthogonal to release (the next opener detects an empty/missing
// file via the lifecycle's adopt-then-create path).
//
// After Close every accessor on *File becomes a programmer error;
// see the lifetime contract on the *File doc.
func (f *File) Close() error {
	if f.mmap != nil {
		if err := munmap(f.mmap); err != nil {
			return fmt.Errorf("lock: munmap: %w", err)
		}
		f.mmap = nil
		f.header = nil
		f.slots = nil
	}
	if f.f != nil {
		if err := f.f.Close(); err != nil {
			return fmt.Errorf("lock: close: %w", err)
		}
		f.f = nil
	}
	return nil
}

// Fd returns the lock file's underlying file descriptor. Used by the
// flock goroutine (chunk 2.4) to issue flock() syscalls. The fd is
// valid until Close. Panics on a closed *File.
func (f *File) Fd() uintptr {
	if f.f == nil {
		panic("lock: Fd on closed *File")
	}
	return f.f.Fd()
}

// MaxReaders returns the lock file's reader-table capacity, read from
// the header. Immutable for the life of the lock file. Panics on a
// closed *File.
func (f *File) MaxReaders() uint32 {
	if f.header == nil {
		panic("lock: MaxReaders on closed *File")
	}
	return Load32(&f.header.MaxReaders)
}

// UUID returns the lock file's UUID (a copy of the data file's UUID
// at creation). Panics on a closed *File.
func (f *File) UUID() [16]byte {
	if f.header == nil {
		panic("lock: UUID on closed *File")
	}
	return f.header.UUID
}

// Slot returns a pointer to slot i. Index must be in [0, MaxReaders).
// The returned pointer references memory inside the mmap region;
// callers MUST use the package-level Load64 / Store64 / CAS64
// helpers on the fields, never plain Go reads/writes. The pointer
// is valid until Close on the *File; using it after Close SIGSEGVs
// the process. See the lifetime contract on *File.
//
// Panics on out-of-range index or a closed *File.
func (f *File) Slot(i uint32) *ReaderSlot {
	if f.slots == nil {
		panic("lock: Slot on closed *File")
	}
	if i >= uint32(len(f.slots)) {
		panic(fmt.Sprintf("lock: slot index %d out of range [0, %d)", i, len(f.slots)))
	}
	return &f.slots[i]
}

// Header field accessors. Each one wraps a single atomic op on the
// underlying mmap-backed field — see cross-process.md §Atomic
// Operations Convention for why typed atomics aren't used. All
// panic on a closed *File.

func (f *File) WriterPID() uint64 {
	if f.header == nil {
		panic("lock: WriterPID on closed *File")
	}
	return Load64(&f.header.WriterPID)
}

func (f *File) SetWriterPID(v uint64) {
	if f.header == nil {
		panic("lock: SetWriterPID on closed *File")
	}
	Store64(&f.header.WriterPID, v)
}

func (f *File) WriterStartTime() uint64 {
	if f.header == nil {
		panic("lock: WriterStartTime on closed *File")
	}
	return Load64(&f.header.WriterStartTime)
}

func (f *File) SetWriterStartTime(v uint64) {
	if f.header == nil {
		panic("lock: SetWriterStartTime on closed *File")
	}
	Store64(&f.header.WriterStartTime, v)
}

func (f *File) WriterPIDNamespace() uint64 {
	if f.header == nil {
		panic("lock: WriterPIDNamespace on closed *File")
	}
	return Load64(&f.header.WriterPIDNamespace)
}

func (f *File) SetWriterPIDNamespace(v uint64) {
	if f.header == nil {
		panic("lock: SetWriterPIDNamespace on closed *File")
	}
	Store64(&f.header.WriterPIDNamespace, v)
}

func (f *File) WriterHeartbeat() uint64 {
	if f.header == nil {
		panic("lock: WriterHeartbeat on closed *File")
	}
	return Load64(&f.header.WriterHeartbeat)
}

func (f *File) SetWriterHeartbeat(v uint64) {
	if f.header == nil {
		panic("lock: SetWriterHeartbeat on closed *File")
	}
	Store64(&f.header.WriterHeartbeat, v)
}

func (f *File) LastMaintenanceTime() uint64 {
	if f.header == nil {
		panic("lock: LastMaintenanceTime on closed *File")
	}
	return Load64(&f.header.LastMaintenanceTime)
}

func (f *File) SetLastMaintenanceTime(v uint64) {
	if f.header == nil {
		panic("lock: SetLastMaintenanceTime on closed *File")
	}
	Store64(&f.header.LastMaintenanceTime, v)
}

// BaseFor returns the conventional lock-file base name for a data
// file whose path is dataPath. The convention appends ".lock" to the
// base name; callers compose with an os.Root over the data file's
// directory (the chunk-1 path-traversal pattern) to assemble the
// full open.
func BaseFor(dataPath string) string {
	return baseName(dataPath) + ".lock"
}

// baseName returns the last path element of dataPath. Avoids
// importing path/filepath for one operation; the lock package's
// other path handling is delegated to os.Root.
func baseName(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' {
			return p[i+1:]
		}
	}
	return p
}

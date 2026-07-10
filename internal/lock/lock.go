package lock

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

// Errors surfaced by the lock-file lifecycle. Sentinels so callers
// can branch via errors.Is.
var (
	// ErrCorrupted is returned when an existing lock file fails
	// validation (Magic mismatch, MaxReaders out of range, file size
	// inconsistent with the header). The lock-file lifecycle treats
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
// (Base) rather than an absolute path. This composes with the
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
// the flock goroutine can invoke flock() on it) plus the
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
	// refs counts lifetime references on the mapping: 1 for the
	// owning handle (seeded at Open, dropped by its Close), plus one
	// per open read transaction (Ref at BeginRead, dropped when the
	// transaction's slot release completes). The munmap happens at
	// the LAST drop, so a read transaction outliving DB.Close keeps
	// its reader slot mapped — and therefore releasable and
	// bound-pinning — until its own close (leak-detection.md
	// §Close() Ordering).
	refs atomic.Int32
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
// A UUID mismatch — and a plausible header whose file size disagrees
// with the current layout (an old-binary lock file; see the size-arm
// comment) — classifies the file STALE: it is unlinked under the
// identity guard (removeStaleGuarded) and the open retries. A Magic /
// MaxReaders validation failure on a finalised file surfaces
// ErrCorrupted without unlinking (the package does not auto-recover
// externally-tampered or crashed-mid-init files).
//
// Retry budget. Three arms consume the 10-attempt budget: adopters
// inside the creator's init window (errPartialInit), contended stale
// removals (errStaleContended — a live legacy coordinator can hold
// the stale inode's flock across the whole budget), and post-verify
// name re-binds (errPathChanged); all three back off. Successful
// stale removals retry immediately. Init is bounded by one Truncate
// + header WriteAt + one fdatasync — typically sub-ms on SSD, up to
// ~100 ms on contended HDDs. The budget + capped exponential backoff
// (1, 2, 4, 8, 16, 32, 64, 128, 256, 256 ms; total ~800 ms) covers a
// slow disk while bounding caller latency.
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
			// The guarded removal already ran inside tryAdoptExisting
			// (identity-verified, under flock on the validated inode —
			// see removeStaleGuarded); retry immediately against the
			// post-removal state.
			lastErr = err
			continue
		}
		if errors.Is(err, errStaleContended) || errors.Is(err, errPathChanged) {
			// Contended removal guard (another remover or a legacy
			// coordinator holds the stale inode's flock), or the name
			// was re-bound between our open/create and the final
			// identity verify. Back off — it de-syncs concurrent
			// validators so a later attempt wins the guard or adopts
			// whatever now lives at the path — then retry.
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
// with backoff. Genuinely corrupt files (Magic non-zero but wrong,
// MaxReaders out of range) still surface ErrCorrupted without retry;
// a size mismatch on a PLAUSIBLE header is classified stale (an
// old-binary layout — see the size-arm comment) and routes through
// the identity-guarded removal, exactly like a UUID mismatch.
//
// Returns os.ErrNotExist (propagated from Root.OpenFile) when the
// file doesn't exist; errStaleUUID when a stale file (UUID or
// layout-size mismatch) was removed — or found already gone /
// re-bound — under the guard; errStaleContended when the guard lost
// the flock conversion; errPathChanged when the post-validation
// identity verify failed; errPartialInit on the creator publish
// window; ErrCorrupted (wrapped) for true Magic / MaxReaders
// mismatches.
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
		// A plausible header (magic + MaxReaders in range) whose file
		// size disagrees with the CURRENT layout is most likely a lock
		// file written by a binary with a different header layout
		// (pre-v1 format changes grow HeaderSize in place). The lock
		// file is transient coordination state Open recreates — treat
		// it as stale, exactly like a UUID mismatch, rather than
		// refusing to open an intact database.
		//
		// Safety invariant: this recreate is sound only because a
		// layout change ships with a data-format-incompatible change
		// (the meta payload layout), so an old-binary peer can never
		// be LIVE on the same data file — it cannot even open it. A
		// future HeaderSize growth WITHOUT a data-format break would
		// make this arm reachable with a live old-binary peer holding
		// the unlinked inode: two lock files, two writers, split
		// brain. Grow the header only alongside a data-format break,
		// or replace this arm first (cross-process.md §Lock File
		// Layout).
		return nil, removeStaleGuarded(p, f)
	}
	if hdr.UUID != p.DataUUID {
		return nil, removeStaleGuarded(p, f)
	}

	// Success: the *File takes ownership of the fd; do not close it
	// on exit. The deferred LOCK_UN still runs (correct — the *File
	// doesn't need flock for steady-state use).
	//
	// Final identity verify (cross-process.md §Lock File Lifecycle):
	// the validated fd must still be what the NAME points at, or we
	// would coordinate on an unlinked inode — invisible to every
	// later opener — while a different lock file governs the data
	// file (split brain). Reachable only through an unguarded
	// remover (an old binary's stale-removal, external tampering);
	// this binary's own removals are identity-guarded.
	if err := verifyPathIdentity(p, f); err != nil {
		return nil, err
	}
	out, err := mmapAndOverlay(f, hdr.MaxReaders, expectedSize)
	if err != nil {
		return nil, err
	}
	closeOnExit = false
	return out, nil
}

// createInitHookForTest is a test-only injection point invoked
// between the creator's O_CREATE|O_EXCL and its first flock(LOCK_EX).
// Production paths leave the pointer nil. Tests use
// SetCreateInitHookForTest to exercise the adopter-side
// errPartialInit retry success branch: a concurrent adopter that
// lands inside the open→flock window must converge via backoff retry,
// not return ErrCorrupted.
//
// Storage is atomic.Pointer so the hook setter races safely against
// concurrent createAndInit calls — required by tests that spawn the
// creator in a goroutine before installing the cleanup.
var createInitHookForTest atomic.Pointer[func()]

// SetCreateInitHookForTest installs (or clears with nil) the
// createAndInit injection point. Tests should restore the prior
// value via t.Cleanup(func() { SetCreateInitHookForTest(nil) }).
func SetCreateInitHookForTest(hook func()) {
	if hook == nil {
		createInitHookForTest.Store(nil)
		return
	}
	createInitHookForTest.Store(&hook)
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
			// real latency cost). Identity-guarded like every other
			// by-name unlink: never remove a file this fd doesn't
			// name (no in-repo path re-binds the name here, but an
			// unguarded remove is exactly the split-brain shape this
			// file exists to prevent).
			if fInfo, ferr := f.Stat(); ferr == nil {
				if pInfo, perr := p.Root.Stat(p.Base); perr == nil && os.SameFile(fInfo, pInfo) {
					_ = p.Root.Remove(p.Base)
				}
			}
		}
	}()

	if hook := createInitHookForTest.Load(); hook != nil {
		(*hook)()
	}

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

	// Final identity verify — same rationale as the adopt path's.
	if err := verifyPathIdentity(p, f); err != nil {
		return nil, err
	}
	out, err := mmapAndOverlay(f, p.MaxReaders, FileSize(p.MaxReaders))
	if err != nil {
		return nil, err
	}
	closeOnExit = false
	removeOnExit = false
	return out, nil
}

// errPathChanged signals that the lock-file NAME was re-bound to a
// different inode between this attempt's open/create and its final
// identity verify. The Open loop retries against the current binding.
var errPathChanged = errors.New("lock: path re-bound during open")

// errStaleContended signals that a stale file's removal guard lost
// the flock conversion (another remover, or a live legacy coordinator
// of the abandoned database, holds the stale inode's flock). The Open
// loop backs off before retrying.
var errStaleContended = errors.New("lock: stale lock file removal contended")

// verifyPathIdentity reports errPathChanged unless the fd still names
// the path's inode.
func verifyPathIdentity(p OpenParams, f *os.File) error {
	fInfo, err := f.Stat()
	if err != nil {
		return fmt.Errorf("lock: fstat %q: %w", p.Base, err)
	}
	pInfo, err := p.Root.Stat(p.Base)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return errPathChanged
		}
		return fmt.Errorf("lock: stat %q: %w", p.Base, err)
	}
	if !os.SameFile(fInfo, pInfo) {
		return errPathChanged
	}
	return nil
}

// staleRemoveHookForTest fires inside removeStaleGuarded after the
// flock upgrade and before the identity re-check — the window a
// concurrent opener's remove-and-recreate must be detected in. Tests
// install it via SetStaleRemoveHookForTest.
var staleRemoveHookForTest atomic.Pointer[func()]

// SetStaleRemoveHookForTest installs (or clears, with nil) the
// stale-removal interleaving hook. Returns a restore func for defer.
// Same convention as SetCreateInitHookForTest.
func SetStaleRemoveHookForTest(hook func()) (restore func()) {
	if hook == nil {
		staleRemoveHookForTest.Store(nil)
		return func() {}
	}
	staleRemoveHookForTest.Store(&hook)
	return func() { staleRemoveHookForTest.Store(nil) }
}

// removeStaleGuarded unlinks a validated-stale lock file by name ONLY
// while holding flock(LOCK_EX) on the validated fd AND after
// re-verifying that the name still points at that inode. An unguarded
// Remove(Base) races a concurrent opener that already removed the
// stale file and created a fresh one: the by-name unlink takes out
// the FRESH file, leaving its creator coordinating on an unlinked
// inode — two lock files, two live writers, meta overwrite (the
// split-brain interleaving this guard exists for). The flock
// serialises concurrent removers (each holds an fd on the same stale
// inode; exactly one upgrades); the identity re-check under the lock
// closes the removed-and-recreated window (a loser sees the name
// bound to a different inode and skips). LOCK_NB, not blocking: a
// blocking upgrade could wait indefinitely on a live legacy
// coordinator of the abandoned database still flocking the stale
// inode — on contention we skip, and the caller's backoff de-syncs
// the retry. flock conversion semantics (Linux, man 2 flock NOTES):
// the held SH is DROPPED first, then EX attempted — a failed NB
// conversion leaves this fd with no lock at all, which is safe here
// (nothing reads the file afterwards; the deferred LOCK_UN is a
// no-op) and is what lets one of two SH-holding removers win the
// second conversion attempt instead of livelocking.
//
// Returns errStaleUUID after a successful removal or a name-gone /
// re-bound skip (the Open loop retries immediately and observes the
// post-removal state), errStaleContended on a lost flock conversion
// (the Open loop backs off first), and a real error for a stat or
// removal failure.
func removeStaleGuarded(p OpenParams, f *os.File) error {
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return errStaleContended // another remover or a legacy holder
	}
	if hook := staleRemoveHookForTest.Load(); hook != nil {
		(*hook)()
	}
	fInfo, err := f.Stat()
	if err != nil {
		return fmt.Errorf("lock: fstat %q under removal guard: %w", p.Base, err)
	}
	pInfo, err := p.Root.Stat(p.Base)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return errStaleUUID // name already gone — nothing to remove
		}
		return fmt.Errorf("lock: stat %q under removal guard: %w", p.Base, err)
	}
	if !os.SameFile(fInfo, pInfo) {
		return errStaleUUID // name re-bound: a concurrent opener already replaced it
	}
	if rmErr := p.Root.Remove(p.Base); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
		return fmt.Errorf("lock: remove stale %q: %w", p.Base, rmErr)
	}
	return errStaleUUID
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
// ErrCorrupted. Manual cleanup is required — gmdb does not
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
	lf := &File{
		f:      f,
		mmap:   mapping,
		header: header,
		slots:  slots,
	}
	lf.refs.Store(1)
	return lf, nil
}

// Ref takes an additional lifetime reference on the mapping (see the
// refs field doc). Must be called while at least one reference is
// held — the closeGate window BeginRead runs under guarantees that.
func (f *File) Ref() {
	if f.refs.Add(1) <= 1 {
		panic("lock: Ref on a fully-closed *File")
	}
}

// Close drops one lifetime reference; the LAST drop releases the mmap
// and closes the file descriptor. The owning handle's teardown and
// each open read transaction's release drop exactly one each, so the
// mapping outlives DB.Close while any read transaction still needs
// its reader slot. Does NOT unlink the lock file — it is ephemeral
// and removal is orthogonal to release (the next opener detects an
// empty/missing file via the lifecycle's adopt-then-create path).
//
// After the final Close every accessor on *File becomes a programmer
// error; see the lifetime contract on the *File doc.
func (f *File) Close() error {
	if f.refs.Add(-1) > 0 {
		return nil
	}
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
// flock goroutine to issue flock() syscalls. The fd is
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

// DataGeneration reads the data-file replacement counter (see the
// field doc in format.go).
func (f *File) DataGeneration() uint64 {
	if f.header == nil {
		panic("lock: DataGeneration on closed *File")
	}
	return Load64(&f.header.DataGeneration)
}

// BumpDataGeneration increments the data-file replacement counter.
// Caller MUST hold the write grant (flock LOCK_EX) — Compact's
// post-rename publish step.
func (f *File) BumpDataGeneration() uint64 {
	if f.header == nil {
		panic("lock: BumpDataGeneration on closed *File")
	}
	return Add64(&f.header.DataGeneration, 1)
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

// TryClaimMaintenance atomically claims the maintenance slot for the
// current interval: if no pass has run within intervalNanos, it CAS-stamps
// LastMaintenanceTime to nowNanos and returns true — the caller owns this
// interval's pass. Otherwise (a recent pass, or a peer won the CAS) it
// returns false and the caller skips. The CAS makes the check-and-claim
// atomic across processes sharing the lock file, so at most one process per
// interval runs maintenance (background-maintenance.md).
//
// On Linux nowNanos is CLOCK_BOOTTIME, which is kernel-wide, so all
// processes share one clock origin and nowNanos >= LastMaintenanceTime
// always holds; the skip test is exact. On platforms whose monotonic clock
// is per-process (darwin/freebsd CLOCK_MONOTONIC) two processes have
// unrelated origins, so a peer's stamp can exceed our nowNanos. The skip
// test is gated on nowNanos >= last precisely so the subtraction never
// wraps: when our clock is behind a peer's stamp we fall through and claim
// (CAS), rather than skipping forever (which would stall maintenance after
// a peer crash). Off-Linux the worst case is a redundant pass (wasted I/O)
// — never a double-reclaim, since the CAS serialises any single instant and
// reclamation is leak-safe (background-maintenance.md).
func (f *File) TryClaimMaintenance(nowNanos, intervalNanos uint64) bool {
	if f.header == nil {
		panic("lock: TryClaimMaintenance on closed *File")
	}
	last := Load64(&f.header.LastMaintenanceTime)
	if last != 0 && nowNanos >= last && nowNanos-last < intervalNanos {
		return false // a pass ran within the interval
	}
	return CAS64(&f.header.LastMaintenanceTime, last, nowNanos)
}

// BaseFor returns the conventional lock-file base name for a data
// file whose path is dataPath. The convention appends ".lock" to the
// base name; callers compose with an os.Root over the data file's
// directory (the path-traversal-safe pattern) to assemble the
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

// LastWriter accessors — the persisted author identity (format.go
// LastWriter*). Written by the flock goroutine at grant acquisition
// under LOCK_EX; LastWriterHeartbeat additionally refreshed by the
// author handle's heartbeat goroutine for its lifetime. NOT cleared at
// grant release — persistence across release is the point.

func (f *File) LastWriterPID() uint64 {
	if f.header == nil {
		panic("lock: LastWriterPID on closed *File")
	}
	return Load64(&f.header.LastWriterPID)
}

func (f *File) SetLastWriterPID(v uint64) {
	if f.header == nil {
		panic("lock: SetLastWriterPID on closed *File")
	}
	Store64(&f.header.LastWriterPID, v)
}

func (f *File) LastWriterStartTime() uint64 {
	if f.header == nil {
		panic("lock: LastWriterStartTime on closed *File")
	}
	return Load64(&f.header.LastWriterStartTime)
}

func (f *File) SetLastWriterStartTime(v uint64) {
	if f.header == nil {
		panic("lock: SetLastWriterStartTime on closed *File")
	}
	Store64(&f.header.LastWriterStartTime, v)
}

func (f *File) LastWriterPIDNamespace() uint64 {
	if f.header == nil {
		panic("lock: LastWriterPIDNamespace on closed *File")
	}
	return Load64(&f.header.LastWriterPIDNamespace)
}

func (f *File) SetLastWriterPIDNamespace(v uint64) {
	if f.header == nil {
		panic("lock: SetLastWriterPIDNamespace on closed *File")
	}
	Store64(&f.header.LastWriterPIDNamespace, v)
}

func (f *File) LastWriterHeartbeat() uint64 {
	if f.header == nil {
		panic("lock: LastWriterHeartbeat on closed *File")
	}
	return Load64(&f.header.LastWriterHeartbeat)
}

func (f *File) SetLastWriterHeartbeat(v uint64) {
	if f.header == nil {
		panic("lock: SetLastWriterHeartbeat on closed *File")
	}
	Store64(&f.header.LastWriterHeartbeat, v)
}

package gmdb

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/thegrumpylion/gmdb/internal/page"
	"github.com/thegrumpylion/gmdb/internal/pager"
)

// DB is a handle to an open gmdb database. Concurrent reads are
// supported once chunk 2 (cross-process coordination) lands; chunk 1
// provides only single-process write semantics via an in-process
// sync.Mutex. Close() releases the mmap and underlying file.
type DB struct {
	file *os.File
	root *os.Root // path-traversal guard from os.OpenRoot

	pool *pager.BufPool
	opts Options

	// Single-process write lock. Chunk 2 layers the cross-process
	// flock + reader-table on top of this.
	writeMu sync.Mutex

	// Pager state for the currently-active reader baseline. mu guards
	// against concurrent Begin from multiple goroutines.
	mu            sync.Mutex
	currentMeta   page.Meta
	activeMetaIdx int
	pgr           *pager.Pager
}

// Open opens the database at path. If the file does not exist, it is
// created with opts; existing files use opts for runtime fields only
// (PageSize, PageChecksum, file-size bounds are taken from the meta
// page).
//
// Open uses os.OpenRoot on `filepath.Dir(path)` so that the data
// file's open and any subsequent in-Root operations are confined to
// that directory — a symlink at `path` resolving outside of it is
// rejected by the Go 1.24+ Root semantics. NB: the path itself is not
// canonicalised; a caller that passes `/foo/../etc/passwd` resolves
// `dir = /etc` and opens `passwd` there. The guard prevents
// symlink-escape from `dir`, not directory-traversal in the input.
func Open(_ context.Context, path string, opts Options) (*DB, error) {
	opts = opts.applyDefaults()
	if err := opts.validate(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidOptions, err)
	}
	dir := filepath.Dir(path)
	base := filepath.Base(path)
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("gmdb: open dir %q: %w", dir, err)
	}
	// Per api-surface.md §Database Initialization: try O_CREATE|O_EXCL
	// first and on EEXIST fall back to the normal-open path. This is
	// the correct ordering against a concurrent Open race — the
	// loser observes EEXIST and proceeds as a regular open of the
	// just-created file.
	file, err := root.OpenFile(base, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	created := true
	if err != nil {
		if !errors.Is(err, os.ErrExist) {
			_ = root.Close()
			return nil, fmt.Errorf("gmdb: create %q: %w", path, err)
		}
		created = false
		file, err = root.OpenFile(base, os.O_RDWR, 0o600)
		if err != nil {
			_ = root.Close()
			return nil, fmt.Errorf("gmdb: open %q: %w", path, err)
		}
	}

	if created {
		ip := pager.InitParams{
			PageSize:        opts.PageSize,
			PageChecksum:    opts.PageChecksum,
			MinSize:         opts.MinSize,
			MaxSize:         opts.MaxSize,
			GrowStep:        opts.GrowStep,
			ShrinkThreshold: opts.ShrinkThreshold,
			UUID:            opts.UUID,
		}
		if err := pager.Init(file, ip); err != nil {
			_ = file.Close()
			_ = root.Close()
			return nil, fmt.Errorf("gmdb: init: %w", err)
		}
	}

	// Read the on-disk meta-0 PageSize so the pool matches the
	// persisted size — opts.PageSize is authoritative only at
	// creation; re-opens honour what was written.
	//
	// If we took the EEXIST-retry fallback, another process may still
	// be in the middle of pager.Init (file exists but metas not yet
	// written). Read meta-0 with bounded retry-on-empty so we don't
	// observe a half-initialised file and fail with a misleading
	// "PageSize invalid" error. Chunk 2's lock file resolves the
	// race structurally; until then this is a chunk-1 defensive
	// measure.
	persistedPageSize, err := readPersistedPageSize(file, !created)
	if err != nil {
		_ = file.Close()
		_ = root.Close()
		return nil, err
	}
	pool := pager.NewBufPool(int(persistedPageSize))
	opened, err := pager.Open(file, pager.OpenParams{
		Pool:             pool,
		MaxTxBufferBytes: opts.MaxTxBufferBytes,
	})
	if err != nil {
		_ = file.Close()
		_ = root.Close()
		return nil, err
	}

	return &DB{
		file:          file,
		root:          root,
		pool:          pool,
		opts:          opts,
		currentMeta:   opened.Meta,
		activeMetaIdx: opened.ActiveMetaIdx,
		pgr:           opened.Pager,
	}, nil
}

// Close releases the mmap and underlying file. After Close, the DB
// handle is unusable.
func (db *DB) Close() error {
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.pgr != nil {
		_ = db.pgr.Close()
		db.pgr = nil
	}
	if db.file != nil {
		_ = db.file.Close()
		db.file = nil
	}
	if db.root != nil {
		_ = db.root.Close()
		db.root = nil
	}
	return nil
}

// Meta returns a snapshot of the currently-active meta. Useful for
// tests; the user-facing Stats API arrives in chunk 11.
func (db *DB) Meta() page.Meta {
	db.mu.Lock()
	defer db.mu.Unlock()
	return db.currentMeta
}

// Begin starts a write transaction. Chunk 1 supports a single writer
// per process; the call blocks on the writeMu until any in-progress
// write tx commits or rolls back. Cross-process serialisation lands in
// chunk 2 via the lock file.
//
// Read transactions are not yet wired (chunk 3 territory); calling
// Begin(write=false) returns ErrReadOnly to signal the unimplemented
// path explicitly.
func (db *DB) Begin(_ context.Context, write bool) (*Tx, error) {
	if !write {
		return nil, ErrReadOnly
	}
	db.writeMu.Lock()
	db.mu.Lock()
	defer db.mu.Unlock()

	prevMeta := db.currentMeta
	prevActive := db.activeMetaIdx
	db.pgr.BeginTx()
	return &Tx{
		db:         db,
		prevMeta:   prevMeta,
		prevActive: prevActive,
		newTxnID:   prevMeta.TxnID + 1,
		writable:   true,
	}, nil
}

// readPersistedPageSize reads meta-0's PageSize field. When we took
// the EEXIST-retry fallback (raceWindow=true), another process may
// still be inside pager.Init; the file exists but bytes 0..143 may
// be zeros. Retry the read with bounded backoff so the loser of an
// O_CREATE|O_EXCL race waits for the winner to finish init.
//
// On a clean (non-race) reopen the first iteration succeeds and the
// retry budget costs one extra ReadAt at most.
func readPersistedPageSize(file *os.File, raceWindow bool) (uint32, error) {
	const (
		maxAttempts = 50
		backoff     = 2 * time.Millisecond
	)
	attempts := 1
	if raceWindow {
		attempts = maxAttempts
	}
	metaPrefix := make([]byte, page.MetaPayloadSize)
	for i := range attempts {
		if _, err := file.ReadAt(metaPrefix, 0); err != nil {
			return 0, fmt.Errorf("gmdb: read meta-0 prefix: %w", err)
		}
		ps := page.DecodeMeta(metaPrefix).PageSize
		if page.ValidPageSize(ps) {
			return ps, nil
		}
		if i+1 < attempts {
			time.Sleep(backoff)
		}
	}
	return 0, fmt.Errorf("gmdb: persisted PageSize invalid (file may be partially initialised by a concurrent Open; chunk-2 lock-file work resolves this)")
}

// Update is a convenience wrapper: begin a write tx, call fn, commit on
// success or rollback on error. If fn returns an error and the rollback
// also fails, both errors are joined via errors.Join so callers can
// inspect either via errors.Is.
func (db *DB) Update(ctx context.Context, fn func(tx *Tx) error) error {
	tx, err := db.Begin(ctx, true)
	if err != nil {
		return err
	}
	if fnErr := fn(tx); fnErr != nil {
		if rbErr := tx.Rollback(); rbErr != nil {
			return errors.Join(fnErr, rbErr)
		}
		return fnErr
	}
	return tx.Commit()
}

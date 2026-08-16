package gmdb

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"

	"github.com/greatliontech/gmdb/internal/bitmap"
	"github.com/greatliontech/gmdb/internal/btree"
	"github.com/greatliontech/gmdb/internal/descriptor"
	"github.com/greatliontech/gmdb/internal/indexing"
	"github.com/greatliontech/gmdb/internal/page"
	"github.com/greatliontech/gmdb/internal/pager"
	"github.com/greatliontech/gmdb/internal/verify"
)

// copyDest is CopyTo's destination-file seam, mirroring the pager's
// FileOps: every write, truncate, and fsync on the temp copy routes
// through it so a test can record the operation order and assert the
// publish invariant (the copy's bytes are complete and fsynced before
// the destination path exists). Production: barrierDest — Sync is the
// platform durability barrier, not a bare fsync.
type copyDest interface {
	io.WriterAt
	Truncate(size int64) error
	Sync() error
}

// copyDestWrapForTest, when set, wraps the destination file handle the
// copy internals write through. Global state — tests that install must
// not run in parallel with other CopyTo/Compact tests.
var copyDestWrapForTest atomic.Pointer[func(copyDest) copyDest]

// copyPublishHookForTest, when set, fires after the temp copy is
// complete and fsynced, immediately before the publish.
var copyPublishHookForTest atomic.Pointer[func(tmpPath string)]

// copyLinkForTest, when set, replaces os.Link in CopyTo's publish step
// — the seam for exercising the non-hard-link destination fallback and
// the NFS link-retransmission quirk without a special filesystem.
// Global state — same non-parallel rule as the other copy seams.
var copyLinkForTest atomic.Pointer[func(oldname, newname string) error]

// barrierDest is the production copyDest: a forward to *os.File whose
// Sync is the platform durability barrier (durability.md §Platform
// sync primitives) honoring Options.NoFullFsync — so the darwin
// full-flush policy covers CopyTo artifact barriers, not only the
// pager's.
type barrierDest struct {
	f           *os.File
	noFullFsync bool
}

func (b barrierDest) WriteAt(p []byte, off int64) (int, error) { return b.f.WriteAt(p, off) }
func (b barrierDest) Truncate(size int64) error                { return b.f.Truncate(size) }
func (b barrierDest) Sync() error                              { return pager.SyncBarrier(b.f, b.noFullFsync) }

// wrapCopyDest builds the production barrierDest and applies the test
// seam around it (identity in production) — a test wrapper forwarding
// Sync still lands on the barrier.
func wrapCopyDest(f *os.File, noFullFsync bool) copyDest {
	dest := copyDest(barrierDest{f: f, noFullFsync: noFullFsync})
	if wrap := copyDestWrapForTest.Load(); wrap != nil {
		return (*wrap)(dest)
	}
	return dest
}

// publicChecksumErr maps the internal pager checksum sentinel — surfaced by
// the compact rebuild's verifying reader on a bitrotted source page — to the
// public gmdb.ErrBadPageChecksum, matching the read-path boundary
// (keyspace.go, tx.go, incremental_compaction.go). Other errors pass through.
func publicChecksumErr(err error) error {
	if err != nil && errors.Is(err, pager.ErrBadPageChecksum) {
		return fmt.Errorf("%w: %w", ErrBadPageChecksum, err)
	}
	return err
}

// CopyTo writes a consistent copy of the database to path, taken from a
// read snapshot — concurrent writers are NOT blocked (api-surface.md
// §Check, CopyTo, Compact). The copy inherits the source's PageSize,
// BitmapPages, and MaxSize; it receives a fresh UUID (a copy is a distinct
// database identity). path must not already exist.
//
// compact=false produces a verbatim copy: every page reachable from the
// snapshot is written at its original page id, and the allocation bitmap
// is REBUILT from the reachable set (rather than copied) so the copy's
// free list is consistent with the snapshot's tree even if a writer
// committed mid-copy — the live on-disk bitmap mutates in place, but the
// snapshot's reachable pages are pinned by reader isolation and stable.
// The copy's RPL is empty: pages the source held pending reader-pinned
// reclamation are unreferenced in the copy and become free space.
//
// compact=true additionally defragments: every B+tree is rebuilt
// bottom-up from its existing entries with sequentially-assigned page ids,
// free pages are omitted, and the file shrinks to the live size. Index
// trees are rebuilt structurally (from their stored entries — the
// extractor closures are not on disk), not re-derived.
//
// The destination is crash-consistent (api-surface.md §Check, CopyTo,
// Compact): the copy is written to a temp file in path's directory,
// fsynced, and only then published at path — a crash mid-copy never
// leaves a partial file at path, only a `<path>.copytmp-*` temp, which
// is inert and safe to delete once no CopyTo is in flight. The publish
// prefers an atomic hard link, which also enforces no-clobber
// atomically (a file appearing at path mid-copy fails the publish
// instead of being overwritten); destinations without hard-link
// support fall back to a no-replace rename — see renameNoReplace for
// the per-filesystem atomicity ladder.
//
// To change file format, re-open the copy and use SetFileFormat.
func (db *DB) CopyTo(path string, compact bool) error {
	rtx, err := db.BeginRead(context.Background())
	if err != nil {
		return err
	}
	defer rtx.Rollback()
	var uuid [16]byte
	if _, err := rand.Read(uuid[:]); err != nil {
		return fmt.Errorf("gmdb: CopyTo generate UUID: %w", err)
	}
	// Fail fast when the destination already exists. Advisory only — the
	// authoritative no-clobber guard is the publish ladder below (link /
	// RENAME_NOREPLACE fail EEXIST rather than overwriting; best-effort
	// on destinations with neither).
	if _, serr := os.Lstat(path); serr == nil {
		return fmt.Errorf("gmdb: CopyTo create %q: %w", path, os.ErrExist)
	} else if !errors.Is(serr, os.ErrNotExist) {
		return fmt.Errorf("gmdb: CopyTo stat %q: %w", path, serr)
	}
	// Write the complete copy at a temp name in path's directory (same
	// filesystem, so the publish link cannot fail EXDEV; the fresh UUID
	// makes the name unique against concurrent CopyTo calls).
	tmp := fmt.Sprintf("%s.copytmp-%x", path, uuid[:8])
	if compact {
		err = publicChecksumErr(copyCompact(rtx,
			func() (*os.File, error) {
				return os.OpenFile(tmp, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
			},
			func() { _ = os.Remove(tmp) },
			uuid))
	} else {
		err = copyVerbatim(rtx, tmp, uuid)
	}
	if err != nil {
		return err
	}
	if hook := copyPublishHookForTest.Load(); hook != nil {
		(*hook)(tmp)
	}
	// Publish: hard-link the complete, fsynced temp at path — atomic and
	// no-clobber (EEXIST if path appeared meanwhile) — make the new
	// dirent durable (durability.md §Directory-entry durability), then
	// drop the temp name. A crash between publish and the dir fsync can
	// lose the dirent but never expose partial bytes: path, when
	// present, always names the fully-fsynced inode.
	osLink := os.Link
	if h := copyLinkForTest.Load(); h != nil {
		osLink = *h
	}
	if lerr := osLink(tmp, path); lerr != nil {
		// A failed link may still have published: on NFS, link(2) can
		// succeed on the server while the client sees an error (the
		// reply is lost; the retransmission hits EEXIST). path already
		// naming the SAME inode as tmp IS the successful publish —
		// treating it as failure would remove the temp and report an
		// error with a complete copy present at path. Anything else
		// routes to the no-replace-rename fallback for destinations
		// without hard-link support (vfat/exfat, many FUSE mounts);
		// its failure — including a genuine EEXIST — leaves any
		// pre-existing path untouched.
		if !publishedByLinkQuirk(tmp, path) {
			if rerr := renameNoReplace(tmp, path); rerr != nil {
				_ = os.Remove(tmp)
				return fmt.Errorf("gmdb: CopyTo publish %q: %w (hard link: %v)", path, rerr, lerr)
			}
		}
	}
	if serr := syncDirPath(filepath.Dir(path), db.opts.NoFullFsync); serr != nil {
		// All-or-nothing: the publish's durability is unknowable after a
		// failed directory fsync, and a caller treating the error as "no
		// backup produced" would otherwise retry into ErrExist forever
		// (or delete a good copy by hand). Unpublish so error ⇒ nothing
		// at path, matching every other CopyTo failure.
		_ = os.Remove(path)
		_ = os.Remove(tmp)
		return fmt.Errorf("gmdb: CopyTo fsync dir: %w", serr)
	}
	_ = os.Remove(tmp)
	return nil
}

// publishedByLinkQuirk reports whether a failed link(2) nevertheless
// published the copy: path already naming the same inode as tmp. The
// reachable producer is NFS's link retransmission (the server applied
// the first request, its reply was lost, and the retransmission
// returned EEXIST) — the one shape where a link "failure" leaves the
// publish semantically complete. Detection is best-effort: a
// transient Lstat failure here (ESTALE on the same flaky mount)
// misses the quirk and routes to the fallback, whose EEXIST error
// then coexists with a complete copy at path — the residual corner
// of an errno-based protocol, strictly narrower than the base
// behavior (which mis-reported EVERY retransmission).
func publishedByLinkQuirk(tmp, path string) bool {
	ti, terr := os.Lstat(tmp)
	pi, perr := os.Lstat(path)
	return terr == nil && perr == nil && os.SameFile(ti, pi)
}

// renameNoReplaceBestEffort is the no-replace publish where no atomic
// form is available: an existence probe, then a plain rename. The
// probe-to-rename window is a genuine TOCTOU race — on these
// destinations the no-clobber guarantee is best-effort, per the
// per-filesystem contract in api-surface.md §Check, CopyTo, Compact.
// The crash-consistency half of the contract is NOT weakened: the
// renamed inode is already complete and fsynced.
func renameNoReplaceBestEffort(tmp, path string) error {
	if _, err := os.Lstat(path); err == nil {
		return &os.LinkError{Op: "rename", Old: tmp, New: path, Err: os.ErrExist}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.Rename(tmp, path)
}

// copyVerbatim implements CopyTo(compact=false): walk the snapshot's
// reachable pages, write each at its original id into a fresh file, and
// rebuild the bitmap + meta. uuid is the copy's database identity (fresh
// for the public CopyTo; the source's for Compact's in-place rebuild).
func copyVerbatim(rtx *ReadTx, path string, uuid [16]byte) error {
	meta := rtx.meta
	cfg := page.Config{PageSize: meta.PageSize, PageChecksum: meta.HasFlag(pager.MetaFlagPageChecksum)}
	hwm := meta.HighWaterMark
	// Clamp the walk/copy bound to the file-resident extent
	// (checksums.md §Structural and Allocation Bounds — the same clamp
	// Check applies): the verbatim path reads through the UNBOUNDED
	// PageRaw, so a source truncated below its meta's HighWaterMark (an
	// incomplete transfer; the meta itself intact) would otherwise
	// SIGBUS on the unbacked tail of the MaxSize mmap reservation. With
	// the clamp, a tree page beyond the extent fails the walk's bound as
	// ErrCorrupted. On a well-formed source the file always covers the
	// HighWaterMark, so the clamp is a no-op. The bound cannot shrink
	// mid-copy: shrink defers while this read snapshot is visible
	// (file-format.md §File Shrinkage).
	if bound := min(uint64(rtx.pgr.FileSize())/uint64(meta.PageSize), meta.MaxSize); hwm > bound {
		hwm = bound
	}
	firstData := uint64(2) + uint64(meta.BitmapPages)

	// 1. Enumerate the reachable page set from the snapshot. Any walk
	// failure (structural corruption) aborts the copy — a corrupt source
	// cannot yield a consistent copy (run Check first to diagnose).
	reachable, err := collectReachable(rtx, cfg, meta, hwm, firstData)
	if err != nil {
		return fmt.Errorf("gmdb: CopyTo enumerate snapshot: %w", err)
	}

	// 2. Create the destination (must not exist) and size it to cover the
	// snapshot's high-water mark (so original page ids fit), with the
	// source's MinSize as a floor.
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("gmdb: CopyTo create %q: %w", path, err)
	}
	// On any error past this point, remove the half-written file. Close
	// before Remove so platforms that refuse to unlink an open handle can
	// still clean up.
	committed := false
	defer func() {
		_ = f.Close()
		if !committed {
			_ = os.Remove(path)
		}
	}()
	dest := wrapCopyDest(f, rtx.db.opts.NoFullFsync)

	filePages := max(hwm, meta.MinSize, firstData)
	pageSize := int64(meta.PageSize)
	if err := dest.Truncate(int64(filePages) * pageSize); err != nil {
		return fmt.Errorf("gmdb: CopyTo truncate: %w", err)
	}

	// 3. Copy each reachable page verbatim at its original id. PageRaw
	// borrows the snapshot mmap; WriteAt copies the bytes out. Verbatim
	// preserves each page's checksum footer (copied to the same id).
	//
	// Prefault the source's file-backed extent first (mmap-strategy.md
	// §Prefaulting: "also performed internally during CopyTo()"): the
	// copy is a full sequential scan, so MADV_POPULATE_READ turns the
	// per-page demand faults into one sequential readahead. Advisory —
	// a silent no-op on kernels < 5.14 / non-Linux.
	_ = rtx.pgr.AdvisePreload(hwm)
	for id := firstData; id < hwm; id++ {
		if !reachable.Test(id) {
			continue
		}
		if _, err := dest.WriteAt(rtx.pgr.PageRaw(id), int64(id)*pageSize); err != nil {
			return fmt.Errorf("gmdb: CopyTo write page %d: %w", id, err)
		}
	}

	// 4. Rebuild the allocation bitmap: every reachable page allocated
	// (bit clear, the zero default), every other data page in
	// [firstData, hwm) free (bit set). Pages >= hwm stay clear, matching a
	// normally-grown database. (bitmap.Set marks FREE.)
	detail := make([]byte, uint64(meta.BitmapPages)*uint64(meta.PageSize))
	bm := bitmap.New(detail, meta.PageSize, meta.BitmapPages, meta.MaxSize)
	for id := firstData; id < hwm; id++ {
		if !reachable.Test(id) {
			bm.Set(id)
		}
	}
	for i := uint64(0); i < uint64(meta.BitmapPages); i++ {
		if _, err := dest.WriteAt(bm.PageBytes(uint32(i)), int64(2+i)*pageSize); err != nil {
			return fmt.Errorf("gmdb: CopyTo write bitmap page %d: %w", i, err)
		}
	}

	// 5. Compose the copy's meta: same tree (KeyspaceRoot, NumKeyspaces,
	// HighWaterMark — ids preserved), fresh UUID, empty RPL, rebuilt free
	// count. Written to both meta slots so either survives a torn write.
	cm := meta
	cm.UUID = uuid
	// The clamped bound, not the source meta's claim: on a well-formed
	// source they are equal; on a clamped (forged-meta) source the copy's
	// HighWaterMark must describe the file the copy actually is.
	cm.HighWaterMark = hwm
	cm.RPLHeadPage = 0
	cm.RPLTailPage = 0
	cm.RPLEntryCount = 0
	cm.NumFreePages = bm.NumFree()
	cm.Flags = meta.Flags & pager.MetaFlagPageChecksum
	// A copy starts its MVCC counter fresh. Both meta slots are written
	// byte-identical at TxnID 0 — the documented post-initialisation
	// tie-at-zero state (pager.ActiveMeta), valid even with a populated
	// KeyspaceRoot. (Equal NON-zero TxnIDs would be a protocol violation.)
	cm.TxnID = 0
	// Like Init's genesis metas, the copy is self-durable at epoch 0
	// (the fsync below makes it so; a fresh RPLHeadTxnID rides the
	// emptied chain). Carrying the SOURCE's sub-record would make a
	// fresh Open of the copy "recover" into the source's geometry.
	cm.RPLHeadTxnID = 0
	cm.Durable = cm.LiveSubRecord()
	cm.Durable.AnchoredTxnID = 0
	metaBuf := make([]byte, meta.PageSize)
	pager.EncodeMeta(metaBuf, &cm)
	for slot := int64(0); slot < 2; slot++ {
		if _, err := dest.WriteAt(metaBuf, slot*pageSize); err != nil {
			return fmt.Errorf("gmdb: CopyTo write meta %d: %w", slot, err)
		}
	}

	// Bytes-durable barrier. Dirent durability for the published name is
	// the caller's publish step (CopyTo publishes then fsyncs the directory);
	// this temp's own dirent needs no durability — a crash leaves only an
	// inert temp file.
	if err := dest.Sync(); err != nil {
		return fmt.Errorf("gmdb: CopyTo fsync: %w", err)
	}
	committed = true
	return nil
}

// collectReachable returns the set of page ids reachable from the
// snapshot's meta: the keyspace B+tree, plus every keyspace's data tree
// (including set-keyspace nested trees and overflow runs, which btree.Walk
// recurses), index registry sub-tree, and index data trees. Mirrors the
// Check structural walk, but records ids only. A walk failure
// (corrupt/forged tree) is returned to the caller.
func collectReachable(rtx *ReadTx, cfg page.Config, meta pager.Meta, hwm, firstData uint64) (verify.Bitset, error) {
	reachable := verify.NewBitset(hwm)
	pr := verify.RawPageReader{P: rtx.pgr}
	collect := func(root uint64) error {
		return btree.Walk(pr, cfg, root, hwm, func(id uint64, _ btree.PageKind, _ int) error {
			reachable.Set(id)
			return nil
		})
	}
	if err := collect(meta.KeyspaceRoot); err != nil {
		return reachable, fmt.Errorf("keyspace tree: %w", err)
	}
	err := btree.WalkKV(pr, cfg, meta.KeyspaceRoot, hwm, func(k, v []byte) error {
		name := string(k)
		if len(v) != descriptor.Size {
			return fmt.Errorf("%w: keyspace %q descriptor size %d", btree.ErrCorrupted, name, len(v))
		}
		desc := descriptor.Decode(v)
		if err := collect(desc.Root); err != nil {
			return fmt.Errorf("keyspace %q data tree: %w", name, err)
		}
		if desc.IndexRegistryRoot == 0 {
			return nil
		}
		if err := collect(desc.IndexRegistryRoot); err != nil {
			return fmt.Errorf("keyspace %q index registry: %w", name, err)
		}
		return btree.WalkKV(pr, cfg, desc.IndexRegistryRoot, hwm, func(ik, iv []byte) error {
			entry, derr := indexing.DecodeRegistryEntry(iv)
			if derr != nil {
				return fmt.Errorf("keyspace %q index %q registry entry: %w", name, string(ik), derr)
			}
			if err := collect(entry.Root); err != nil {
				return fmt.Errorf("keyspace %q index %q data tree: %w", name, string(ik), err)
			}
			return nil
		})
	})
	return reachable, err
}

// freshFileWriter is the bulkPageWriter + bulkOverflowWriter for CopyTo's
// compacting rebuild: it allocates page ids sequentially from firstData and
// pwrites fully-formed pages into the destination file. It shares the
// bottom-up builders (bulkBuilder / setBulk / bulkLeafEntry) with
// BulkLoad — the only difference is the destination (a fresh file rather
// than the live pager).
type freshFileWriter struct {
	f        copyDest
	pageSize int64
	checksum bool
	next     uint64 // next page id to allocate (starts at firstData)
	maxPages uint64 // MaxSize — the allocation ceiling
}

// AllocContiguous reserves n sequential page ids. There is no free list
// during a rebuild — ids are handed out monotonically, so the final value
// of next is the copy's HighWaterMark.
func (w *freshFileWriter) AllocContiguous(n uint32) (uint64, error) {
	if w.next+uint64(n) > w.maxPages {
		return 0, ErrDBFull
	}
	id := w.next
	w.next += uint64(n)
	return id, nil
}

func (w *freshFileWriter) AllocPage() (uint64, error) { return w.AllocContiguous(1) }

// WriteDirect writes a fully-formed page at id's offset, stamping the
// xxhash footer in place first when checksums are enabled (matching the
// pager's WriteDirect). buf must be exactly one page.
func (w *freshFileWriter) WriteDirect(id uint64, buf []byte) error {
	if w.checksum {
		page.WritePageFooter(buf, uint32(w.pageSize))
	}
	return w.WriteDirectRaw(id, buf)
}

// WriteDirectRaw writes a fully-formed page with NO footer stamp — for
// overflow-run pages, whose integrity cover is the head-resident
// whole-run digest, not per-page footers (checksums.md §Overflow-Run
// Digest; matching the pager's WriteDirectRaw).
func (w *freshFileWriter) WriteDirectRaw(id uint64, buf []byte) error {
	if _, err := w.f.WriteAt(buf, int64(id)*w.pageSize); err != nil {
		return fmt.Errorf("gmdb: CopyTo write page %d: %w", id, err)
	}
	return nil
}

// copyCompact implements CopyTo(compact=true): rebuild every B+tree
// bottom-up into a fresh file with sequential page ids, omitting free
// pages. Each keyspace's data tree, index registry, and index data trees
// are rebuilt from their existing (sorted) entries — index trees
// structurally, since the extractor closures are not on disk — and a new
// keyspace-descriptor tree is built with the rewritten roots. The result
// is a defragmented, minimally-sized copy with an all-allocated bitmap.
// copyCompact's destination is caller-addressed: createDest must
// create-exclusive the temp and removeDest must remove it, both in
// the SAME namespace the caller later publishes from — Compact
// creates, removes, and renames through db.root so one re-pointed
// path component cannot make the publish pick up a different file
// than the copy wrote; CopyTo addresses its user-supplied path
// directly.
func copyCompact(rtx *ReadTx, createDest func() (*os.File, error), removeDest func(), uuid [16]byte) error {
	meta := rtx.meta
	// The read pager's cfg is decode-only (PageSize, checksum) — decoding
	// dispatches on the type byte and needs no builder defaults. The
	// rebuild BUILDS pages, so graft the engine-wide builder defaults
	// from Options onto it; descriptor.ApplyToConfig then applies the
	// per-keyspace overrides below, exactly as the live write path does.
	baseCfg := rtx.pgr.Config()
	baseCfg.RestartGroupTarget = rtx.db.opts.RestartGroupTarget
	baseCfg.LeafLayout = page.LeafLayout(rtx.db.opts.LeafLayout)
	baseCfg.BranchLayout = page.BranchLayout(rtx.db.opts.BranchLayout)
	hwm := meta.HighWaterMark
	firstData := uint64(2) + uint64(meta.BitmapPages)
	pageSize := int64(meta.PageSize)
	// Verifying reader (checksums.md §Verification): the compact rebuild
	// DECODES source pages and re-encodes them into fresh pages with a new
	// footer, so an unverified read would launder a bitrotted-but-decodable
	// source page into a valid-checksummed copy — converting a detectable
	// ErrBadPageChecksum into a permanent silent wrong value. Verifying here
	// makes a bad footer abort the rebuild; the !committed defer removes the
	// half-built file, so the corruption stays detectable on the original.
	// (The verbatim CopyTo path keeps verify.RawPageReader: it copies footers
	// byte-for-byte, so corruption survives detectably without a re-encode.)
	pr := verify.VerifyingPageReader{P: rtx.pgr}

	f, err := createDest()
	if err != nil {
		return fmt.Errorf("gmdb: CopyTo create temp: %w", err)
	}
	committed := false
	defer func() {
		_ = f.Close()
		if !committed {
			removeDest()
		}
	}()

	w := &freshFileWriter{
		f:        wrapCopyDest(f, rtx.db.opts.NoFullFsync),
		pageSize: pageSize,
		checksum: meta.HasFlag(pager.MetaFlagPageChecksum),
		next:     firstData,
		maxPages: meta.MaxSize,
	}

	// Rebuild each keyspace's trees, collecting the rewritten descriptors in
	// keyspace-name order (WalkKV enumerates the source descriptor tree
	// sorted, so the collected order is already the order the new descriptor
	// tree needs).
	type descEntry struct {
		name []byte
		enc  []byte
	}
	var descs []descEntry
	walkErr := btree.WalkKV(pr, baseCfg, meta.KeyspaceRoot, hwm, func(k, v []byte) error {
		name := string(k)
		if len(v) != descriptor.Size {
			return fmt.Errorf("%w: keyspace %q descriptor size %d", btree.ErrCorrupted, name, len(v))
		}
		desc := descriptor.Decode(v)
		cfg := descriptor.ApplyToConfig(desc, baseCfg)
		nd := desc // rewrite Root + IndexRegistryRoot below

		switch desc.Kind {
		case descriptor.KindKeyspace:
			root, err := rebuildKVTree(w, pr, cfg, desc.Root, hwm)
			if err != nil {
				return fmt.Errorf("keyspace %q data tree: %w", name, err)
			}
			nd.Root = root
		case descriptor.KindSetKeyspace:
			root, err := rebuildSetTree(w, pr, cfg, desc.Root, desc.FixedValueSize, hwm)
			if err != nil {
				return fmt.Errorf("set keyspace %q data tree: %w", name, err)
			}
			nd.Root = root
		default:
			return fmt.Errorf("%w: keyspace %q has kind %d, which CopyTo(compact) cannot rebuild", btree.ErrCorrupted, name, desc.Kind)
		}

		if desc.IndexRegistryRoot != 0 {
			// The registry sub-tree and index data trees are rebuilt with
			// the BASE cfg (not the keyspace-overridden one): the runtime
			// maintains them with tx.pgr.Config() (index_codec.go registryPut
			// / index_maintain.go), so matching it keeps the compacted index
			// leaf-compression shape identical to a Put-maintained index. (The
			// data tree above correctly uses the keyspace-overridden cfg.)
			regRoot, err := rebuildRegistry(w, pr, baseCfg, desc.IndexRegistryRoot, hwm)
			if err != nil {
				return fmt.Errorf("keyspace %q index registry: %w", name, err)
			}
			nd.IndexRegistryRoot = regRoot
		}

		encBuf := make([]byte, descriptor.Size)
		descriptor.Encode(encBuf, nd)
		descs = append(descs, descEntry{name: append([]byte(nil), k...), enc: encBuf})
		return nil
	})
	if walkErr != nil {
		return fmt.Errorf("gmdb: CopyTo(compact) rebuild: %w", walkErr)
	}

	// Build the new keyspace descriptor tree from the rewritten descriptors.
	var newKeyspaceRoot uint64
	if len(descs) > 0 {
		db := newBulkBuilder(w, baseCfg)
		for _, d := range descs {
			if err := db.add(page.LeafEntry{Key: d.name, Value: d.enc}, d.name); err != nil {
				return fmt.Errorf("gmdb: CopyTo(compact) descriptor tree: %w", err)
			}
		}
		newKeyspaceRoot, _, err = db.finish()
		if err != nil {
			return fmt.Errorf("gmdb: CopyTo(compact) descriptor tree finish: %w", err)
		}
	}

	finalHWM := w.next
	filePages := max(finalHWM, meta.MinSize, firstData)
	if err := w.f.Truncate(int64(filePages) * pageSize); err != nil {
		return fmt.Errorf("gmdb: CopyTo(compact) truncate: %w", err)
	}

	// Bitmap: every data page in [firstData, finalHWM) is allocated (the
	// zero-detail default); the rebuild left no free pages. NumFreePages=0.
	detail := make([]byte, uint64(meta.BitmapPages)*uint64(meta.PageSize))
	bm := bitmap.New(detail, meta.PageSize, meta.BitmapPages, meta.MaxSize)
	for i := uint64(0); i < uint64(meta.BitmapPages); i++ {
		if _, err := w.f.WriteAt(bm.PageBytes(uint32(i)), int64(2+i)*pageSize); err != nil {
			return fmt.Errorf("gmdb: CopyTo(compact) write bitmap page %d: %w", i, err)
		}
	}

	cm := meta
	cm.UUID = uuid
	cm.KeyspaceRoot = newKeyspaceRoot
	cm.NumKeyspaces = uint64(len(descs))
	cm.HighWaterMark = finalHWM
	cm.RPLHeadPage = 0
	cm.RPLTailPage = 0
	cm.RPLEntryCount = 0
	cm.NumFreePages = bm.NumFree()
	cm.Flags = meta.Flags & pager.MetaFlagPageChecksum
	cm.TxnID = 0 // fresh MVCC counter; both slots at the post-init tie-at-zero state
	// Self-durable at epoch 0, like Init and the verbatim copy above.
	cm.RPLHeadTxnID = 0
	cm.Durable = cm.LiveSubRecord()
	cm.Durable.AnchoredTxnID = 0
	metaBuf := make([]byte, meta.PageSize)
	pager.EncodeMeta(metaBuf, &cm)
	for slot := int64(0); slot < 2; slot++ {
		if _, err := w.f.WriteAt(metaBuf, slot*pageSize); err != nil {
			return fmt.Errorf("gmdb: CopyTo(compact) write meta %d: %w", slot, err)
		}
	}

	// Bytes-durable barrier; dirent durability is the caller's concern
	// (CopyTo's publish step, or Compact's post-rename directory fsync).
	if err := w.f.Sync(); err != nil {
		return fmt.Errorf("gmdb: CopyTo(compact) fsync: %w", err)
	}
	committed = true
	return nil
}

// rebuildKVTree rebuilds a plain key→value B+tree (a Keyspace data tree or
// an index data tree) bottom-up from its existing sorted entries, writing
// into w. Overflow values are re-streamed (bulkLeafEntry re-promotes the
// assembled value to a fresh overflow chain). Returns the new root id.
func rebuildKVTree(w *freshFileWriter, pr btree.PageReader, cfg page.Config, root, hwm uint64) (uint64, error) {
	if root == 0 {
		return 0, nil
	}
	b := newBulkBuilder(w, cfg)
	err := btree.WalkKV(pr, cfg, root, hwm, func(k, v []byte) error {
		e, err := bulkLeafEntry(w, cfg, k, v)
		if err != nil {
			return err
		}
		return b.add(e, k)
	})
	if err != nil {
		return 0, err
	}
	newRoot, _, err := b.finish()
	return newRoot, err
}

// rebuildRegistry rebuilds a keyspace's index registry sub-tree: each
// registry entry's index data tree is rebuilt (rebuildKVTree), the entry's
// Root is rewritten to the new tree, and the re-encoded entry is added to a
// fresh registry tree. Returns the new registry root id.
func rebuildRegistry(w *freshFileWriter, pr btree.PageReader, cfg page.Config, regRoot, hwm uint64) (uint64, error) {
	if regRoot == 0 {
		return 0, nil
	}
	b := newBulkBuilder(w, cfg)
	err := btree.WalkKV(pr, cfg, regRoot, hwm, func(k, v []byte) error {
		entry, derr := indexing.DecodeRegistryEntry(v)
		if derr != nil {
			return fmt.Errorf("index %q registry entry: %w", string(k), derr)
		}
		newRoot, rerr := rebuildKVTree(w, pr, cfg, entry.Root, hwm)
		if rerr != nil {
			return fmt.Errorf("index %q data tree: %w", string(k), rerr)
		}
		entry.Root = newRoot
		// Decode→mutate(Root/Count)→re-encode round-trip: the
		// uint16-bounded fields came off disk within bound and are
		// not mutated, so ErrFieldTooLarge is unreachable; if ever
		// reached (memory corruption) the raw internal error is the
		// right class — no ErrInvalidOptions mapping here.
		nv, eerr := indexing.EncodeRegistryEntry(entry)
		if eerr != nil {
			return fmt.Errorf("index %q re-encode: %w", string(k), eerr)
		}
		return b.add(page.LeafEntry{Key: k, Value: nv}, k)
	})
	if err != nil {
		return 0, err
	}
	newRoot, _, err := b.finish()
	return newRoot, err
}

// rebuildSetTree rebuilds a SetKeyspace's outer tree bottom-up. For each
// set key it re-accumulates the member set through setBulk (the same
// subpage-or-nested-tree promotion the per-Put and BulkLoad paths use), so
// the rebuilt storage shape is re-optimised, not merely copied. Members
// arrive in sorted order from the source (subpage members and nested-tree
// keys are both stored sorted), satisfying the builder's ascending-key
// precondition. Returns the new outer root id.
func rebuildSetTree(w *freshFileWriter, pr btree.PageReader, cfg page.Config, root uint64, fvs uint16, hwm uint64) (uint64, error) {
	if root == 0 {
		return 0, nil
	}
	sb := &setBulk{
		top:       newBulkBuilder(w, cfg),
		pw:        w,
		cfg:       cfg,
		fvs:       fvs,
		threshold: page.SubpagePromotionThreshold(cfg),
	}
	err := btree.WalkLeafEntries(pr, cfg, root, hwm, func(e page.LeafEntry) error {
		// The raw walk yields resident-only keys for overflow-key
		// outer entries; the rebuild re-accumulates by FULL key or the
		// rebuilt set keys would be silently truncated.
		outerKey := e.Key
		if e.IsOverflowKey() {
			full, kerr := btree.MaterializeEntryKey(pr, cfg, e)
			if kerr != nil {
				return kerr
			}
			outerKey = full
		}
		sb.startKey(outerKey)
		switch {
		case e.IsSubpage():
			if len(e.Value) < page.SubpageHeaderSize {
				return fmt.Errorf("%w: set subpage for key %q is %d bytes (< header %d)",
					btree.ErrCorrupted, e.Key, len(e.Value), page.SubpageHeaderSize)
			}
			sp := page.NewSubpageReader(e.Value, fvs)
			if verr := sp.Validate(); verr != nil {
				return fmt.Errorf("%w: set subpage for key %q: %w", btree.ErrCorrupted, e.Key, verr)
			}
			var inErr error
			sp.AllValues(func(member []byte) bool {
				if aerr := sb.addValue(member); aerr != nil {
					inErr = aerr
					return false
				}
				return true
			})
			if inErr != nil {
				return inErr
			}
		case e.IsNestedTree():
			if werr := btree.WalkKV(pr, cfg, e.NestedRoot, hwm, func(member, _ []byte) error {
				return sb.addValue(member)
			}); werr != nil {
				return werr
			}
		default:
			return fmt.Errorf("%w: set entry for key %q is neither subpage nor nested-tree (flags 0x%x)",
				btree.ErrCorrupted, e.Key, e.Flags)
		}
		return sb.flush()
	})
	if err != nil {
		return 0, err
	}
	newRoot, _, err := sb.top.finish()
	return newRoot, err
}

package gmdb

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"

	"github.com/thegrumpylion/gmdb/internal/bitmap"
	"github.com/thegrumpylion/gmdb/internal/btree"
	"github.com/thegrumpylion/gmdb/internal/page"
)

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
	if compact {
		return copyCompact(rtx, path, uuid)
	}
	return copyVerbatim(rtx, path, uuid)
}

// copyVerbatim implements CopyTo(compact=false): walk the snapshot's
// reachable pages, write each at its original id into a fresh file, and
// rebuild the bitmap + meta. uuid is the copy's database identity (fresh
// for the public CopyTo; the source's for Compact's in-place rebuild).
func copyVerbatim(rtx *ReadTx, path string, uuid [16]byte) error {
	meta := rtx.meta
	cfg := page.Config{PageSize: meta.PageSize, PageChecksum: meta.HasFlag(page.MetaFlagPageChecksum)}
	hwm := meta.HighWaterMark
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

	filePages := max(hwm, meta.MinSize, firstData)
	pageSize := int64(meta.PageSize)
	if err := f.Truncate(int64(filePages) * pageSize); err != nil {
		return fmt.Errorf("gmdb: CopyTo truncate: %w", err)
	}

	// 3. Copy each reachable page verbatim at its original id. PageRaw
	// borrows the snapshot mmap; WriteAt copies the bytes out. Verbatim
	// preserves each page's checksum footer (copied to the same id).
	for id := firstData; id < hwm; id++ {
		if !reachable.test(id) {
			continue
		}
		if _, err := f.WriteAt(rtx.pgr.PageRaw(id), int64(id)*pageSize); err != nil {
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
		if !reachable.test(id) {
			bm.Set(id)
		}
	}
	for i := uint64(0); i < uint64(meta.BitmapPages); i++ {
		if _, err := f.WriteAt(bm.PageBytes(uint32(i)), int64(2+i)*pageSize); err != nil {
			return fmt.Errorf("gmdb: CopyTo write bitmap page %d: %w", i, err)
		}
	}

	// 5. Compose the copy's meta: same tree (KeyspaceRoot, NumKeyspaces,
	// HighWaterMark — ids preserved), fresh UUID, empty RPL, rebuilt free
	// count. Written to both meta slots so either survives a torn write.
	cm := meta
	cm.UUID = uuid
	cm.RPLHeadPage = 0
	cm.RPLTailPage = 0
	cm.RPLEntryCount = 0
	cm.NumFreePages = bm.NumFree()
	cm.Flags = (meta.Flags & page.MetaFlagPageChecksum) | page.MetaFlagCheckpoint
	// A copy starts its MVCC counter fresh. Both meta slots are written
	// byte-identical at TxnID 0 — the documented post-initialisation
	// tie-at-zero state (page.ActiveMeta), valid even with a populated
	// KeyspaceRoot. (Equal NON-zero TxnIDs would be a protocol violation.)
	cm.TxnID = 0
	metaBuf := make([]byte, meta.PageSize)
	page.EncodeMeta(metaBuf, &cm)
	for slot := int64(0); slot < 2; slot++ {
		if _, err := f.WriteAt(metaBuf, slot*pageSize); err != nil {
			return fmt.Errorf("gmdb: CopyTo write meta %d: %w", slot, err)
		}
	}

	if err := f.Sync(); err != nil {
		return fmt.Errorf("gmdb: CopyTo fsync: %w", err)
	}
	committed = true
	return nil
}

// collectReachable returns the set of page ids reachable from the
// snapshot's meta: the keyspace B+tree, plus every keyspace's data tree
// (including set-keyspace nested trees and overflow runs, which btree.Walk
// recurses), index registry sub-tree, and index data trees. Mirrors the
// chunk-11.2 Check structural walk, but records ids only. A walk failure
// (corrupt/forged tree) is returned to the caller.
func collectReachable(rtx *ReadTx, cfg page.Config, meta page.Meta, hwm, firstData uint64) (bitset, error) {
	reachable := newBitset(hwm)
	pr := rawPageReader{p: rtx.pgr}
	collect := func(root uint64) error {
		return btree.Walk(pr, cfg, root, hwm, func(id uint64, _ btree.PageKind, _ int) error {
			reachable.set(id)
			return nil
		})
	}
	if err := collect(meta.KeyspaceRoot); err != nil {
		return reachable, fmt.Errorf("keyspace tree: %w", err)
	}
	err := btree.WalkKV(pr, cfg, meta.KeyspaceRoot, hwm, func(k, v []byte) error {
		name := string(k)
		if len(v) != page.KeyspaceDescriptorSize {
			return fmt.Errorf("%w: keyspace %q descriptor size %d", btree.ErrCorrupted, name, len(v))
		}
		desc := page.DecodeKeyspaceDescriptor(v)
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
			entry, derr := decodeRegistryEntry(iv)
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
// chunk-8 bottom-up builders (bulkBuilder / setBulk / bulkLeafEntry) with
// BulkLoad — the only difference is the destination (a fresh file rather
// than the live pager).
type freshFileWriter struct {
	f        *os.File
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
func copyCompact(rtx *ReadTx, path string, uuid [16]byte) error {
	meta := rtx.meta
	baseCfg := rtx.pgr.Config()
	hwm := meta.HighWaterMark
	firstData := uint64(2) + uint64(meta.BitmapPages)
	pageSize := int64(meta.PageSize)
	pr := rawPageReader{p: rtx.pgr}

	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("gmdb: CopyTo create %q: %w", path, err)
	}
	committed := false
	defer func() {
		_ = f.Close()
		if !committed {
			_ = os.Remove(path)
		}
	}()

	w := &freshFileWriter{
		f:        f,
		pageSize: pageSize,
		checksum: meta.HasFlag(page.MetaFlagPageChecksum),
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
		if len(v) != page.KeyspaceDescriptorSize {
			return fmt.Errorf("%w: keyspace %q descriptor size %d", btree.ErrCorrupted, name, len(v))
		}
		desc := page.DecodeKeyspaceDescriptor(v)
		cfg := baseCfg
		if desc.RestartGroupTarget != 0 {
			cfg.RestartGroupTarget = desc.RestartGroupTarget
		}
		nd := desc // rewrite Root + IndexRegistryRoot below

		switch desc.Kind {
		case page.KeyspaceKindKeyspace:
			root, err := rebuildKVTree(w, pr, cfg, desc.Root, hwm)
			if err != nil {
				return fmt.Errorf("keyspace %q data tree: %w", name, err)
			}
			nd.Root = root
		case page.KeyspaceKindSetKeyspace:
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

		encBuf := make([]byte, page.KeyspaceDescriptorSize)
		page.EncodeKeyspaceDescriptor(encBuf, nd)
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
			if err := db.add(page.LeafEntry{Key: d.name, Value: d.enc}); err != nil {
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
	if err := f.Truncate(int64(filePages) * pageSize); err != nil {
		return fmt.Errorf("gmdb: CopyTo(compact) truncate: %w", err)
	}

	// Bitmap: every data page in [firstData, finalHWM) is allocated (the
	// zero-detail default); the rebuild left no free pages. NumFreePages=0.
	detail := make([]byte, uint64(meta.BitmapPages)*uint64(meta.PageSize))
	bm := bitmap.New(detail, meta.PageSize, meta.BitmapPages, meta.MaxSize)
	for i := uint64(0); i < uint64(meta.BitmapPages); i++ {
		if _, err := f.WriteAt(bm.PageBytes(uint32(i)), int64(2+i)*pageSize); err != nil {
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
	cm.Flags = (meta.Flags & page.MetaFlagPageChecksum) | page.MetaFlagCheckpoint
	cm.TxnID = 0 // fresh MVCC counter; both slots at the post-init tie-at-zero state
	metaBuf := make([]byte, meta.PageSize)
	page.EncodeMeta(metaBuf, &cm)
	for slot := int64(0); slot < 2; slot++ {
		if _, err := f.WriteAt(metaBuf, slot*pageSize); err != nil {
			return fmt.Errorf("gmdb: CopyTo(compact) write meta %d: %w", slot, err)
		}
	}

	if err := f.Sync(); err != nil {
		return fmt.Errorf("gmdb: CopyTo(compact) fsync: %w", err)
	}
	committed = true
	return nil
}

// rebuildKVTree rebuilds a plain key→value B+tree (a Keyspace data tree or
// an index data tree) bottom-up from its existing sorted entries, writing
// into w. Overflow values are re-streamed (bulkLeafEntry re-promotes the
// assembled value to a fresh overflow chain). Returns the new root id.
func rebuildKVTree(w *freshFileWriter, pr rawPageReader, cfg page.Config, root, hwm uint64) (uint64, error) {
	if root == 0 {
		return 0, nil
	}
	b := newBulkBuilder(w, cfg)
	err := btree.WalkKV(pr, cfg, root, hwm, func(k, v []byte) error {
		e, err := bulkLeafEntry(w, cfg, k, v)
		if err != nil {
			return err
		}
		return b.add(e)
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
func rebuildRegistry(w *freshFileWriter, pr rawPageReader, cfg page.Config, regRoot, hwm uint64) (uint64, error) {
	if regRoot == 0 {
		return 0, nil
	}
	b := newBulkBuilder(w, cfg)
	err := btree.WalkKV(pr, cfg, regRoot, hwm, func(k, v []byte) error {
		entry, derr := decodeRegistryEntry(v)
		if derr != nil {
			return fmt.Errorf("index %q registry entry: %w", string(k), derr)
		}
		newRoot, rerr := rebuildKVTree(w, pr, cfg, entry.Root, hwm)
		if rerr != nil {
			return fmt.Errorf("index %q data tree: %w", string(k), rerr)
		}
		entry.Root = newRoot
		nv, eerr := encodeRegistryEntry(entry)
		if eerr != nil {
			return fmt.Errorf("index %q re-encode: %w", string(k), eerr)
		}
		return b.add(page.LeafEntry{Key: k, Value: nv})
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
func rebuildSetTree(w *freshFileWriter, pr rawPageReader, cfg page.Config, root uint64, fvs uint16, hwm uint64) (uint64, error) {
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
		sb.startKey(e.Key)
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

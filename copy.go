package gmdb

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"

	"github.com/thegrumpylion/gmdb/internal/bitmap"
	"github.com/thegrumpylion/gmdb/internal/btree"
	"github.com/thegrumpylion/gmdb/internal/page"
)

// errCompactCopyPending is the placeholder returned by CopyTo(compact=true)
// until the bottom-up rebuild lands (plan 11.5b). It is NOT a public
// sentinel — callers should not depend on it.
var errCompactCopyPending = errors.New("gmdb: CopyTo(compact=true) not yet implemented")

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
// compact=true additionally defragments (rebuilds every tree bottom-up,
// omitting free pages); it is implemented in a following change set.
//
// To change file format, re-open the copy and use SetFileFormat.
func (db *DB) CopyTo(path string, compact bool) error {
	rtx, err := db.BeginRead(context.Background())
	if err != nil {
		return err
	}
	defer rtx.Rollback()
	if compact {
		return errCompactCopyPending
	}
	var uuid [16]byte
	if _, err := rand.Read(uuid[:]); err != nil {
		return fmt.Errorf("gmdb: CopyTo generate UUID: %w", err)
	}
	return copyVerbatim(rtx, path, uuid)
}

// copyVerbatim implements CopyTo(compact=false): walk the snapshot's
// reachable pages, write each at its original id into a fresh file, and
// rebuild the bitmap + meta. uuid is the copy's database identity (fresh
// for the public CopyTo; the source's for Compact's in-place rebuild).
func copyVerbatim(rtx *ReadTx, path string, uuid [16]byte) error {
	meta := rtx.Meta()
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
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o644)
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

package gmdb

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/greatliontech/gmdb/internal/btree"
	"github.com/greatliontech/gmdb/internal/pager"
)

// A verbatim CopyTo of a source whose file was truncated below the
// meta's HighWaterMark (an incomplete transfer; the meta itself is
// intact) must return an error — never SIGBUS through the unbacked
// tail of the MaxSize mmap reservation (checksums.md §Structural and
// Allocation Bounds: the walk bound must track the file-resident
// extent, exactly as Check clamps it).
func TestCopyToTruncatedSourceErrors(t *testing.T) {
	ctx := context.Background()
	src := tmpPath(t)
	db, err := Open(ctx, src, Options{PageSize: 4096, MinSize: 16, MaxSize: 256})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := db.Update(ctx, func(tx *Tx) error {
		ks, e := tx.CreateKeyspace("k")
		if e != nil {
			return e
		}
		return ks.Put([]byte("a"), []byte("b"))
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Truncate away every data page: metas (pages 0-1) and the bitmap
	// page survive, the keyspace tree does not. The meta still claims
	// the pre-truncation HighWaterMark.
	const firstData = 3 // 2 metas + 1 bitmap page (MaxSize 256 → 1 page)
	if err := os.Truncate(src, firstData*4096); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	db, err = Open(ctx, src, Options{PageSize: 4096, MinSize: 16, MaxSize: 256})
	if err != nil {
		t.Fatalf("reopen truncated: %v", err)
	}
	defer db.Close()
	dst := tmpPath(t)
	err = db.CopyTo(dst, false)
	if err == nil {
		t.Fatal("CopyTo of a truncated source succeeded; want a structural error")
	}
	if !errors.Is(err, btree.ErrCorrupted) && !errors.Is(err, ErrCorrupted) {
		t.Fatalf("CopyTo = %v, want a corruption-class error", err)
	}
	if _, serr := os.Lstat(dst); !errors.Is(serr, os.ErrNotExist) {
		t.Fatalf("failed CopyTo left something at the destination: %v", serr)
	}
}

// Check must report a corrupted overflow-run first-page header — the
// read path rejects the run at assembly (DecodeOverflowFirstPage), so a
// walk that never cross-checks the header passes a database clean while
// every Get of the key fails ErrCorrupted. Checksums are disabled so
// the footer cannot mask the structural gap (the finding's in-spec
// reachable class: DisablePageChecksum or a recomputed footer).
func TestCheckReportsCorruptOverflowHeader(t *testing.T) {
	for _, tc := range []struct {
		name       string
		wrongCount bool
	}{{"wrong_type", false}, {"wrong_count", true}} {
		t.Run(tc.name, func(t *testing.T) {
			testCheckReportsCorruptOverflowHeader(t, tc.wrongCount)
		})
	}
}

func testCheckReportsCorruptOverflowHeader(t *testing.T, wrongCount bool) {
	ctx := context.Background()
	path := tmpPath(t)
	db, err := Open(ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 256, DisablePageChecksum: true})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	big := bytes.Repeat([]byte{0xab}, 9000) // 3-page overflow run
	if err := db.Update(ctx, func(tx *Tx) error {
		ks, e := tx.CreateKeyspace("k")
		if e != nil {
			return e
		}
		return ks.Put([]byte("big"), big)
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Find the overflow run's first page (TypeOverflow=3 header) and
	// corrupt it: the type byte for the wrong-type case, the
	// AdditionalPages count (header bytes 4-8) for the wrong-count case.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	corrupted := false
	for off := 0; off+4096 <= len(raw); off += 4096 {
		if raw[off] == 3 { // page.TypeOverflow
			if wrongCount {
				raw[off+4] = raw[off+4] + 1 // AdditionalPages LSB
			} else {
				raw[off] = 9 // no such type
			}
			corrupted = true
			break
		}
	}
	if !corrupted {
		t.Fatal("no overflow page found in the file (fixture broken)")
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write back: %v", err)
	}

	db, err = Open(ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 256, DisablePageChecksum: true})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db.Close()

	// The read path already rejects the run.
	rerr := db.View(ctx, func(rtx *ReadTx) error {
		ks, e := rtx.OpenKeyspaceReadOnly("k")
		if e != nil {
			return e
		}
		_, e = ks.Get([]byte("big"))
		return e
	})
	if !errors.Is(rerr, ErrCorrupted) {
		t.Fatalf("Get over the corrupt overflow header = %v, want ErrCorrupted", rerr)
	}

	// Check must agree with the read path.
	found := false
	for issue := range db.Check() {
		if issue.Severity >= CheckError && strings.Contains(issue.Message, "overflow") {
			found = true
		}
	}
	if !found {
		t.Fatal("Check reported no overflow-header issue on a database whose Get fails ErrCorrupted (false negative)")
	}

	// Stats shares the guarded walk, so it errors too (rather than
	// blindly counting a run the read path rejects).
	serr := db.View(ctx, func(rtx *ReadTx) error {
		ks, e := rtx.OpenKeyspaceReadOnly("k")
		if e != nil {
			return e
		}
		_, e = ks.Stats()
		return e
	})
	if !errors.Is(serr, ErrCorrupted) {
		t.Fatalf("Stats over the corrupt overflow header = %v, want ErrCorrupted", serr)
	}
}

// CopyTo publishes atomically: the destination path must not exist
// until the temp copy is complete and fsynced, the last destination-file
// operation before publish must be the fsync, and a successful CopyTo
// leaves no temp residue (api-surface.md §Check, CopyTo, Compact:
// destination crash-consistency).
func TestCopyToPublishAtomicity(t *testing.T) {
	ctx := context.Background()
	src := tmpPath(t)
	db, err := Open(ctx, src, Options{PageSize: 4096, MinSize: 16, MaxSize: 256})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	if err := db.Update(ctx, func(tx *Tx) error {
		ks, e := tx.CreateKeyspace("k")
		if e != nil {
			return e
		}
		return ks.Put([]byte("a"), []byte("val"))
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	for _, compact := range []bool{false, true} {
		name := "verbatim"
		if compact {
			name = "compact"
		}
		t.Run(name, func(t *testing.T) {
			dst := tmpPath(t)
			rec := &copyDestRecorder{}
			restoreDest := SetCopyDestWrapForTest(rec.wrap)
			defer restoreDest()
			hookRan := false
			restoreHook := SetCopyPublishHookForTest(func(tmp string) {
				hookRan = true
				// Nothing at the destination path yet.
				if _, err := os.Lstat(dst); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("destination exists before publish: %v", err)
				}
				// The temp is inside the destination's directory (same
				// filesystem — the hard link cannot fail EXDEV).
				if filepath.Dir(tmp) != filepath.Dir(dst) {
					t.Errorf("temp %q not in destination directory %q", tmp, filepath.Dir(dst))
				}
				// The bytes are complete and durable: the recorder saw the
				// fsync as the LAST destination-file operation.
				last := rec.lastOp()
				if last != "sync" {
					t.Errorf("last destination op before publish = %q, want sync", last)
				}
				// The temp is a complete, openable copy.
				tdb, err := Open(ctx, tmp, Options{PageSize: 4096, MinSize: 16, MaxSize: 256, ReadOnly: true})
				if err != nil {
					t.Errorf("temp does not open as a complete copy: %v", err)
					return
				}
				// The lock file is THIS open's residue, not CopyTo's —
				// drop it so the no-residue assertion below sees only
				// what CopyTo itself left behind.
				defer os.Remove(tmp + ".lock")
				// The verification open also mints the temp lock
				// file's readers directory on the per-slot lock-FILE
				// tier; both are this hook's residue, not CopyTo's.
				defer func() {
					if ms, _ := filepath.Glob(tmp + ".lock.readers-*"); ms != nil {
						for _, m := range ms {
							_ = os.RemoveAll(m)
						}
					}
				}()
				defer tdb.Close()
				if err := tdb.View(ctx, func(rtx *ReadTx) error {
					ks, e := rtx.OpenKeyspaceReadOnly("k")
					if e != nil {
						return e
					}
					v, e := ks.Get([]byte("a"))
					if e != nil {
						return e
					}
					if string(v) != "val" {
						t.Errorf("temp copy value = %q", v)
					}
					return nil
				}); err != nil {
					t.Errorf("temp copy read: %v", err)
				}
			})
			defer restoreHook()

			if err := db.CopyTo(dst, compact); err != nil {
				t.Fatalf("CopyTo: %v", err)
			}
			if !hookRan {
				t.Fatal("publish hook never ran")
			}
			// No temp residue after success.
			matches, err := filepath.Glob(dst + ".copytmp-*")
			if err != nil {
				t.Fatalf("glob: %v", err)
			}
			if len(matches) != 0 {
				t.Fatalf("temp residue after successful CopyTo: %v", matches)
			}
			// The published copy opens and reads.
			cdb, err := Open(ctx, dst, Options{PageSize: 4096, MinSize: 16, MaxSize: 256})
			if err != nil {
				t.Fatalf("open copy: %v", err)
			}
			defer cdb.Close()
			if err := cdb.View(ctx, func(rtx *ReadTx) error {
				ks, e := rtx.OpenKeyspaceReadOnly("k")
				if e != nil {
					return e
				}
				v, e := ks.Get([]byte("a"))
				if e != nil {
					return e
				}
				if string(v) != "val" {
					t.Errorf("copy value = %q", v)
				}
				return nil
			}); err != nil {
				t.Fatalf("copy read: %v", err)
			}
		})
	}
}

// copyDestRecorder wraps the copy destination file, recording the kind
// of each operation so the publish test can assert fsync-last ordering.
type copyDestRecorder struct {
	inner copyDest
	ops   []string
}

func (r *copyDestRecorder) wrap(d copyDest) copyDest {
	r.inner = d
	r.ops = nil
	return r
}

func (r *copyDestRecorder) lastOp() string {
	if len(r.ops) == 0 {
		return ""
	}
	return r.ops[len(r.ops)-1]
}

func (r *copyDestRecorder) WriteAt(p []byte, off int64) (int, error) {
	r.ops = append(r.ops, "write")
	return r.inner.WriteAt(p, off)
}

func (r *copyDestRecorder) Truncate(size int64) error {
	r.ops = append(r.ops, "truncate")
	return r.inner.Truncate(size)
}

func (r *copyDestRecorder) Sync() error {
	r.ops = append(r.ops, "sync")
	return r.inner.Sync()
}

// A forged HighWaterMark above the file-resident extent, with the tree
// itself intact below it, must not propagate into the copy: the
// verbatim copy clamps its walk AND stamps the clamped bound as the
// copy's HighWaterMark, so the copy describes the file it actually is.
func TestCopyToForgedHWMClampsCopyMeta(t *testing.T) {
	ctx := context.Background()
	src := tmpPath(t)
	db, err := Open(ctx, src, Options{PageSize: 4096, MinSize: 16, MaxSize: 256})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := db.Update(ctx, func(tx *Tx) error {
		ks, e := tx.CreateKeyspace("k")
		if e != nil {
			return e
		}
		return ks.Put([]byte("a"), []byte("b"))
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Forge both meta slots' HighWaterMark far above the real file
	// extent (but under MaxSize). The tree is untouched below it.
	raw, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	for slot := 0; slot < 2; slot++ {
		m := pager.DecodeMeta(raw[slot*4096 : (slot+1)*4096])
		m.HighWaterMark = 200 // file is ~16 pages; MaxSize 256
		pager.EncodeMeta(raw[slot*4096:(slot+1)*4096], &m)
	}
	if err := os.WriteFile(src, raw, 0o600); err != nil {
		t.Fatalf("write back: %v", err)
	}

	db, err = Open(ctx, src, Options{PageSize: 4096, MinSize: 16, MaxSize: 256})
	if err != nil {
		t.Fatalf("reopen forged: %v", err)
	}
	defer db.Close()
	dst := tmpPath(t)
	if err := db.CopyTo(dst, false); err != nil {
		t.Fatalf("CopyTo: %v", err)
	}

	cdb, err := Open(ctx, dst, Options{PageSize: 4096, MinSize: 16, MaxSize: 256})
	if err != nil {
		t.Fatalf("open copy: %v", err)
	}
	defer cdb.Close()
	for issue := range cdb.Check() {
		if issue.Code == "HighWaterMarkOutOfRange" {
			t.Fatalf("the copy inherited the forged HighWaterMark: %s", issue.Message)
		}
	}
}

// The no-clobber guard is the atomic link itself, not only the fail-fast
// existence check: a file appearing at path AFTER CopyTo started (past
// the fail-fast, before the publish) fails the publish with the file
// untouched — a rename-style publish would silently clobber it.
func TestCopyToPublishNeverClobbersLateFile(t *testing.T) {
	ctx := context.Background()
	src := tmpPath(t)
	db, err := Open(ctx, src, Options{PageSize: 4096, MinSize: 16, MaxSize: 256})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	if err := db.Update(ctx, func(tx *Tx) error {
		ks, e := tx.CreateKeyspace("k")
		if e != nil {
			return e
		}
		return ks.Put([]byte("a"), []byte("b"))
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	dst := tmpPath(t)
	sentinel := []byte("late arrival — must survive")
	restore := SetCopyPublishHookForTest(func(string) {
		if err := os.WriteFile(dst, sentinel, 0o600); err != nil {
			t.Fatalf("plant late file: %v", err)
		}
	})
	defer restore()

	if err := db.CopyTo(dst, false); !errors.Is(err, os.ErrExist) {
		t.Fatalf("CopyTo with a late-appearing destination = %v, want an ErrExist-class publish failure", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if !bytes.Equal(got, sentinel) {
		t.Fatal("the late-appearing file was clobbered by the publish")
	}
	// The failed publish cleaned its temp.
	matches, err := filepath.Glob(dst + ".copytmp-*")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("temp residue after failed publish: %v", matches)
	}
}

// A directory-fsync failure after the publish link UNPUBLISHES: an
// error return from CopyTo always means nothing was produced at path
// (api-surface.md destination crash-consistency invariant) — otherwise
// a caller retrying on error wedges on ErrExist forever, or deletes
// what is actually a good copy.
func TestCopyToDirSyncFailureUnpublishes(t *testing.T) {
	ctx := context.Background()
	src := tmpPath(t)
	db, err := Open(ctx, src, Options{PageSize: 4096, MinSize: 16, MaxSize: 256})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	if err := db.Update(ctx, func(tx *Tx) error {
		ks, e := tx.CreateKeyspace("k")
		if e != nil {
			return e
		}
		return ks.Put([]byte("a"), []byte("b"))
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	dst := tmpPath(t)
	injected := errors.New("injected dir fsync failure")
	restore := SetSyncDirHookForTest(func(string) error { return injected })
	err = db.CopyTo(dst, false)
	restore()
	if !errors.Is(err, injected) {
		t.Fatalf("CopyTo = %v, want the injected dir-fsync failure", err)
	}
	if _, serr := os.Lstat(dst); !errors.Is(serr, os.ErrNotExist) {
		t.Fatalf("failed CopyTo left the copy published at path: %v", serr)
	}
	matches, gerr := filepath.Glob(dst + ".copytmp-*")
	if gerr != nil {
		t.Fatalf("glob: %v", gerr)
	}
	if len(matches) != 0 {
		t.Fatalf("temp residue after failed CopyTo: %v", matches)
	}

	// A retry with the failure gone succeeds — the failed publish left
	// no wedge.
	if err := db.CopyTo(dst, false); err != nil {
		t.Fatalf("retry CopyTo: %v", err)
	}
}

// copyToSrcDB builds a one-key source DB for publish-path tests.
func copyToSrcDB(t *testing.T, ctx context.Context) *DB {
	t.Helper()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 256})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Update(ctx, func(tx *Tx) error {
		ks, e := tx.CreateKeyspace("k")
		if e != nil {
			return e
		}
		return ks.Put([]byte("a"), []byte("b"))
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	return db
}

// verifyPublishedCopy opens dst and checks the copied row.
func verifyPublishedCopy(t *testing.T, ctx context.Context, dst string) {
	t.Helper()
	cp, err := Open(ctx, dst, Options{PageSize: 4096, MinSize: 16, MaxSize: 256})
	if err != nil {
		t.Fatalf("open copy: %v", err)
	}
	defer cp.Close()
	rtx, _ := cp.BeginRead(ctx)
	defer rtx.Rollback()
	ks, err := rtx.OpenKeyspaceReadOnly("k")
	if err != nil {
		t.Fatalf("OpenKeyspaceReadOnly: %v", err)
	}
	v, err := ks.Get([]byte("a"))
	if err != nil || !bytes.Equal(v, []byte("b")) {
		t.Fatalf("copy Get = (%q, %v), want (b, nil)", v, err)
	}
}

// A destination filesystem without hard-link support (link(2) →
// ENOTSUP: vfat/exfat, many FUSE mounts) publishes via the
// no-replace-rename fallback (api-surface.md §Check, CopyTo,
// Compact per-filesystem contract).
func TestCopyToPublishFallbackRenameWhenLinkUnsupported(t *testing.T) {
	ctx := context.Background()
	db := copyToSrcDB(t, ctx)
	restore := SetCopyLinkForTest(func(_, newname string) error {
		return &os.LinkError{Op: "link", Old: "", New: newname, Err: errors.ErrUnsupported}
	})
	defer restore()
	dst := tmpPath(t)
	if err := db.CopyTo(dst, false); err != nil {
		t.Fatalf("CopyTo over linkless destination: %v", err)
	}
	verifyPublishedCopy(t, ctx, dst)
	matches, err := filepath.Glob(dst + ".copytmp-*")
	if err != nil || len(matches) != 0 {
		t.Fatalf("temp not cleaned after rename publish: %v %v", matches, err)
	}
}

// The rename fallback preserves the no-clobber contract: a file
// appearing at path mid-copy fails the publish (ErrExist-class) with
// the pre-existing file untouched — never replaced.
func TestCopyToPublishFallbackPreservesNoClobber(t *testing.T) {
	ctx := context.Background()
	db := copyToSrcDB(t, ctx)
	dst := tmpPath(t)
	sentinel := []byte("late arrival — must survive the fallback")
	restoreHook := SetCopyPublishHookForTest(func(string) {
		if err := os.WriteFile(dst, sentinel, 0o600); err != nil {
			t.Fatalf("plant late file: %v", err)
		}
	})
	defer restoreHook()
	restoreLink := SetCopyLinkForTest(func(_, newname string) error {
		return &os.LinkError{Op: "link", Old: "", New: newname, Err: errors.ErrUnsupported}
	})
	defer restoreLink()
	if err := db.CopyTo(dst, false); !errors.Is(err, os.ErrExist) {
		t.Fatalf("fallback publish over late file = %v, want ErrExist-class", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil || !bytes.Equal(got, sentinel) {
		t.Fatalf("late file clobbered by the fallback: (%q, %v)", got, err)
	}
	matches, err := filepath.Glob(dst + ".copytmp-*")
	if err != nil || len(matches) != 0 {
		t.Fatalf("temp not cleaned after failed publish: %v %v", matches, err)
	}
}

// NFS link retransmission: link(2) reports failure although the server
// applied it — path already names the copied inode. That IS the
// publish; CopyTo must succeed instead of removing the temp and
// reporting an error with a complete copy present.
func TestCopyToPublishNFSRetransmissionQuirk(t *testing.T) {
	ctx := context.Background()
	db := copyToSrcDB(t, ctx)
	restore := SetCopyLinkForTest(func(oldname, newname string) error {
		if err := os.Link(oldname, newname); err != nil {
			return err
		}
		// The server applied the link; the client saw a lost reply.
		return &os.LinkError{Op: "link", Old: oldname, New: newname, Err: errors.New("nfs: retransmission timeout")}
	})
	defer restore()
	dst := tmpPath(t)
	if err := db.CopyTo(dst, false); err != nil {
		t.Fatalf("CopyTo with retransmission-quirk link: %v", err)
	}
	verifyPublishedCopy(t, ctx, dst)
	matches, err := filepath.Glob(dst + ".copytmp-*")
	if err != nil || len(matches) != 0 {
		t.Fatalf("temp not cleaned after quirk publish: %v %v", matches, err)
	}
}

// renameNoReplaceBestEffort unit contract: refuses an existing path
// (ErrExist-class, path untouched), renames into an absent one.
func TestRenameNoReplaceBestEffort(t *testing.T) {
	dir := t.TempDir()
	tmp := filepath.Join(dir, "tmp")
	path := filepath.Join(dir, "dst")
	if err := os.WriteFile(tmp, []byte("copy"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("existing"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := renameNoReplaceBestEffort(tmp, path); !errors.Is(err, os.ErrExist) {
		t.Fatalf("best-effort over existing = %v, want ErrExist", err)
	}
	got, _ := os.ReadFile(path)
	if !bytes.Equal(got, []byte("existing")) {
		t.Fatal("existing path clobbered")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := renameNoReplaceBestEffort(tmp, path); err != nil {
		t.Fatalf("best-effort into absent path = %v", err)
	}
	got, _ = os.ReadFile(path)
	if !bytes.Equal(got, []byte("copy")) {
		t.Fatal("rename did not publish the copy")
	}
}

// The dir-fsync-failure unpublish holds over the RENAME publish too:
// error ⇒ nothing at path, no temp residue (the temp name was
// consumed by the rename; its removal is an ENOENT no-op), retry
// succeeds.
func TestCopyToDirSyncFailureUnpublishesRenamePublish(t *testing.T) {
	ctx := context.Background()
	db := copyToSrcDB(t, ctx)
	restoreLink := SetCopyLinkForTest(func(_, newname string) error {
		return &os.LinkError{Op: "link", Old: "", New: newname, Err: errors.ErrUnsupported}
	})
	defer restoreLink()
	dst := tmpPath(t)
	injected := errors.New("injected dir fsync failure")
	restoreSync := SetSyncDirHookForTest(func(string) error { return injected })
	err := db.CopyTo(dst, false)
	restoreSync()
	if !errors.Is(err, injected) {
		t.Fatalf("CopyTo = %v, want the injected dir-fsync failure", err)
	}
	if _, serr := os.Lstat(dst); !errors.Is(serr, os.ErrNotExist) {
		t.Fatalf("failed CopyTo left the rename-published copy at path: %v", serr)
	}
	matches, gerr := filepath.Glob(dst + ".copytmp-*")
	if gerr != nil || len(matches) != 0 {
		t.Fatalf("temp residue after failed CopyTo: %v %v", matches, gerr)
	}
	if err := db.CopyTo(dst, false); err != nil {
		t.Fatalf("retry CopyTo (rename publish): %v", err)
	}
	verifyPublishedCopy(t, ctx, dst)
}

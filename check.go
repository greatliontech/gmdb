package gmdb

import (
	"context"
	"errors"
	"fmt"
	"iter"

	"github.com/thegrumpylion/gmdb/internal/bitmap"
	"github.com/thegrumpylion/gmdb/internal/btree"
	"github.com/thegrumpylion/gmdb/internal/page"
)

// CheckSeverity grades a CheckIssue.
type CheckSeverity int

const (
	// CheckWarning is a non-critical finding (e.g. a leaked-but-harmless
	// page, a free-count mismatch under concurrent writes).
	CheckWarning CheckSeverity = iota
	// CheckError is a structural integrity violation (bad checksum,
	// malformed page, a reachable page the bitmap marks free).
	CheckError
	// CheckFatal marks a point past which the walk could not continue;
	// it is always the last issue yielded.
	CheckFatal
)

// CheckIssue is one finding from a Check walk. See api-surface.md
// §Check, CopyTo, Compact. Code is a stable machine-parseable token;
// Message is free-form human-facing text (do not pattern-match on it).
type CheckIssue struct {
	Severity CheckSeverity
	Code     string
	PageID   uint64
	Keyspace string
	Index    string
	Message  string
	Repaired bool
}

// Check performs a structural integrity walk over a read snapshot and
// returns the findings as an iter.Seq[CheckIssue]. It verifies the
// active meta, every reachable B+tree page's checksum (when
// PageChecksum is enabled) and structure, keyspace-descriptor
// consistency, set-keyspace nested-tree integrity, the RPL chain, and
// the allocation-bitmap page accounting (leaked + reachable-but-free
// pages). Walk failures are reported as CheckFatal and are always the
// last issue yielded.
//
// Check opens its read transaction lazily inside the returned iterator
// and releases the reader slot via defer when the range loop finishes —
// whether it runs to completion or the caller breaks early. A caller
// that never ranges over the result opens no transaction.
//
// Page accounting (leaked / free-count findings) is exact only when no
// writer commits during the walk: Check reads the live on-disk bitmap,
// which a concurrent commit advances past the snapshot's TxnID, so
// under concurrent writes those findings are advisory (a page a newer
// writer allocated looks unreferenced against the older snapshot's
// tree). Run Check on a quiescent database, or use the exclusive Repair
// path, for authoritative accounting.
func (db *DB) Check() iter.Seq[CheckIssue] {
	return func(yield func(CheckIssue) bool) {
		rtx, err := db.BeginRead(context.Background())
		if err != nil {
			yield(CheckIssue{
				Severity: CheckFatal,
				Code:     "ReadTxUnavailable",
				Message:  fmt.Sprintf("Check could not open a read snapshot: %v", err),
			})
			return
		}
		defer rtx.Rollback()
		meta := rtx.Meta()
		c := &checker{
			rtx:   rtx,
			cfg:   page.Config{PageSize: meta.PageSize, PageChecksum: meta.HasFlag(page.MetaFlagPageChecksum)},
			meta:  meta,
			yield: yield,
		}
		c.run()
	}
}

// errCheckStop is returned by a checker's visit callback to abort an
// in-progress btree.Walk when the caller stopped iterating.
var errCheckStop = errors.New("check: iteration stopped by caller")

type checker struct {
	rtx   *ReadTx
	cfg   page.Config
	meta  page.Meta
	yield func(CheckIssue) bool

	stopped   bool
	reachable bitset
}

// emit yields one issue. Returns false (and latches stopped) when the
// caller has asked to stop iterating.
func (c *checker) emit(iss CheckIssue) bool {
	if c.stopped {
		return false
	}
	if !c.yield(iss) {
		c.stopped = true
		return false
	}
	return true
}

func (c *checker) run() {
	hwm := c.meta.HighWaterMark
	// Defend against a forged meta with HighWaterMark beyond what the
	// file actually covers: the reader mmap reservation is MaxSize pages
	// of ADDRESS SPACE but the FILE is only fileSize bytes, so a page id
	// in [filePages, MaxSize) would SIGBUS, and sizing the reachable
	// bitset to a forged-huge HWM/MaxSize would OOM. ValidateMeta does
	// not bound these, so clamp the walk to the real on-disk page count.
	// (Inv-C1 no-crash.)
	bound := min(uint64(c.rtx.pgr.FileSize())/uint64(c.cfg.PageSize), c.meta.MaxSize)
	if hwm > bound {
		c.emit(CheckIssue{Severity: CheckError, Code: "HighWaterMarkOutOfRange",
			Message: fmt.Sprintf("meta HighWaterMark %d exceeds file/MaxSize bound %d; walk clamped", hwm, bound)})
		hwm = bound
	}
	firstData := uint64(2) + uint64(c.meta.BitmapPages)
	c.reachable = newBitset(hwm)

	if err := page.ValidateMeta(c.meta); err != nil {
		if !c.emit(CheckIssue{Severity: CheckError, Code: "MetaInvalid",
			Message: fmt.Sprintf("active meta invalid: %v", err)}) {
			return
		}
	}

	// Walk the top-level keyspace B+tree, then every keyspace's data
	// tree, index registry, and index data trees.
	if !c.walkTree("", "", c.meta.KeyspaceRoot, firstData, hwm) {
		return
	}
	if c.meta.KeyspaceRoot != 0 {
		if !c.walkKeyspaces(firstData, hwm) {
			return
		}
	}

	// RPL chain → set of pages pending reclamation.
	rplPages, ok := c.walkRPL(hwm)
	if !ok {
		return
	}

	c.accounting(firstData, hwm, rplPages)
}

// walkKeyspaces enumerates keyspaces from the snapshot's KeyspaceRoot,
// validates each descriptor, and walks each keyspace's data tree +
// index registry + index data trees into the reachable set. Enumeration
// uses the hwm-guarded btree.WalkKV (NOT the read cursor, whose descent
// is unguarded and would panic/SIGBUS on a corrupt or forged tree —
// Check must not crash on the corruption it exists to detect).
func (c *checker) walkKeyspaces(firstData, hwm uint64) bool {
	err := btree.WalkKV(c.rtx.pgr, c.cfg, c.meta.KeyspaceRoot, hwm, func(k, v []byte) error {
		name := string(k)
		if len(v) != page.KeyspaceDescriptorSize {
			if !c.emit(CheckIssue{Severity: CheckError, Code: "KeyspaceDescriptorSize", Keyspace: name,
				Message: fmt.Sprintf("descriptor value length %d != %d", len(v), page.KeyspaceDescriptorSize)}) {
				return errCheckStop
			}
			return nil
		}
		desc := page.DecodeKeyspaceDescriptor(v)
		if verr := page.ValidateKeyspaceDescriptor(v, desc); verr != nil {
			if !c.emit(CheckIssue{Severity: CheckError, Code: "KeyspaceDescriptorInvalid", Keyspace: name,
				Message: verr.Error()}) {
				return errCheckStop
			}
			return nil
		}
		if !c.walkTree(name, "", desc.Root, firstData, hwm) {
			return errCheckStop
		}
		if desc.IndexRegistryRoot != 0 {
			if !c.walkRegistry(name, desc, firstData, hwm) {
				return errCheckStop
			}
		}
		return nil
	})
	return c.dispositionEnumErr(err, "KeyspaceWalkFailed", "", "keyspace enumeration")
}

// walkRegistry walks a keyspace's index registry sub-tree (registry
// pages) and, for each registry entry, the index's data tree. Uses the
// guarded WalkKV for the same reason as walkKeyspaces.
func (c *checker) walkRegistry(ks string, desc page.KeyspaceDescriptor, firstData, hwm uint64) bool {
	if !c.walkTree(ks, "", desc.IndexRegistryRoot, firstData, hwm) {
		return false
	}
	err := btree.WalkKV(c.rtx.pgr, c.cfg, desc.IndexRegistryRoot, hwm, func(k, v []byte) error {
		idxName := string(k)
		entry, derr := decodeRegistryEntry(v)
		if derr != nil {
			if !c.emit(CheckIssue{Severity: CheckError, Code: "RegistryEntryInvalid", Keyspace: ks, Index: idxName,
				Message: fmt.Sprintf("registry entry decode: %v", derr)}) {
				return errCheckStop
			}
			return nil
		}
		if !c.walkTree(ks, idxName, entry.Root, firstData, hwm) {
			return errCheckStop
		}
		return nil
	})
	return c.dispositionEnumErr(err, "RegistryWalkFailed", ks, "index registry enumeration")
}

// dispositionEnumErr maps a WalkKV result: nil → true (continue);
// errCheckStop → false (caller stopped); a structural failure → a
// CheckFatal issue, after which the whole walk halts so the fatal is the
// LAST issue yielded (api-surface.md §Check). It latches stopped and
// returns false so run() does not proceed to walkRPL / accounting (whose
// findings would otherwise follow the fatal, and would be spurious since
// the aborted enumeration left the reachable set incomplete).
func (c *checker) dispositionEnumErr(err error, code, ks, what string) bool {
	if err == nil {
		return true
	}
	if errors.Is(err, errCheckStop) {
		return false
	}
	c.emit(CheckIssue{Severity: CheckFatal, Code: code, Keyspace: ks,
		Message: fmt.Sprintf("%s failed: %v", what, err)})
	c.stopped = true
	return false
}

// walkTree walks one B+tree rooted at root, verifying each page's
// checksum + structure and recording it in the reachable set. A walk
// failure (corrupt/forged page) is reported as a CheckError for ks/idx
// and does not abort the overall Check (the next tree still runs).
// Returns false only when the caller stopped iterating.
func (c *checker) walkTree(ks, idx string, root, firstData, hwm uint64) bool {
	if root == 0 {
		return true
	}
	visit := func(id uint64, kind btree.PageKind, depth int) error {
		if id < firstData {
			if !c.emit(CheckIssue{Severity: CheckError, Code: "PointerIntoReservedRegion", PageID: id, Keyspace: ks, Index: idx,
				Message: fmt.Sprintf("tree page %d lies in the reserved meta/bitmap region (< %d)", id, firstData)}) {
				return errCheckStop
			}
			return nil
		}
		if c.reachable.test(id) {
			if !c.emit(CheckIssue{Severity: CheckError, Code: "PageDoubleReferenced", PageID: id, Keyspace: ks, Index: idx,
				Message: fmt.Sprintf("page %d is reachable from more than one parent", id)}) {
				return errCheckStop
			}
			return nil
		}
		c.reachable.set(id)
		if c.cfg.PageChecksum {
			if !page.VerifyPageFooter(c.rtx.pgr.Page(id), c.cfg.PageSize) {
				if !c.emit(CheckIssue{Severity: CheckError, Code: "BadPageChecksum", PageID: id, Keyspace: ks, Index: idx,
					Message: fmt.Sprintf("page %d checksum mismatch", id)}) {
					return errCheckStop
				}
			}
		}
		return nil
	}
	err := btree.Walk(c.rtx.pgr, c.cfg, root, hwm, visit)
	if err == nil {
		return true
	}
	if errors.Is(err, errCheckStop) {
		return false
	}
	// A structural walk failure: report and continue with other trees.
	sev := CheckError
	code := "TreeCorrupted"
	if errors.Is(err, btree.ErrTreeTooDeep) {
		code = "TreeCycleOrTooDeep"
	}
	return c.emit(CheckIssue{Severity: sev, Code: code, Keyspace: ks, Index: idx,
		Message: fmt.Sprintf("tree walk from root %d: %v", root, err)})
}

// walkRPL walks the snapshot's RPL chain head→tail, decoding each
// segment, detecting cycles, and returning the set of pages pending
// reclamation (the segment pages themselves + every entry they list).
// Returns ok=false only when the caller stopped iterating.
func (c *checker) walkRPL(hwm uint64) (bitset, bool) {
	pending := newBitset(hwm)
	id := c.meta.RPLHeadPage
	maxSegs := c.meta.RPLEntryCount + 1
	visited := make(map[uint64]struct{})
	var segs uint64
	for id != 0 {
		if id >= hwm {
			return pending, c.emit(CheckIssue{Severity: CheckError, Code: "RPLSegmentOutOfRange", PageID: id,
				Message: fmt.Sprintf("RPL segment page %d >= HighWaterMark %d", id, hwm)})
		}
		if _, seen := visited[id]; seen {
			return pending, c.emit(CheckIssue{Severity: CheckError, Code: "RPLChainCycle", PageID: id,
				Message: fmt.Sprintf("RPL chain cycle at segment page %d", id)})
		}
		if segs > maxSegs {
			return pending, c.emit(CheckIssue{Severity: CheckError, Code: "RPLChainTooLong", PageID: id,
				Message: fmt.Sprintf("RPL chain exceeds entry-count bound %d (likely cycle)", maxSegs)})
		}
		visited[id] = struct{}{}
		seg, ok := page.DecodeRPLSegment(c.rtx.pgr.Page(id), c.cfg)
		if !ok {
			return pending, c.emit(CheckIssue{Severity: CheckError, Code: "RPLSegmentMalformed", PageID: id,
				Message: fmt.Sprintf("RPL segment page %d malformed", id)})
		}
		pending.setIfInRange(id)
		for _, pid := range seg.PageIDs {
			pending.setIfInRange(pid)
		}
		segs++
		id = seg.OlderSegment
	}
	return pending, true
}

// accounting compares the reachable + RPL-pending sets against the
// snapshot's allocation bitmap for every data page in [firstData, hwm):
// a reachable page the bitmap marks free is a ReachableButFree error; an
// allocated page that is neither reachable nor RPL-pending is a
// BitmapLeak warning. (Inv-C2 page-accounting partition.)
func (c *checker) accounting(firstData, hwm uint64, rplPages bitset) {
	bm, ok := c.snapshotBitmap()
	if !ok {
		c.emit(CheckIssue{Severity: CheckWarning, Code: "BitmapUnavailable",
			Message: "could not read allocation bitmap from snapshot; page accounting skipped"})
		return
	}
	for id := firstData; id < hwm; id++ {
		free := bm.IsSet(id) // true = free
		reach := c.reachable.test(id)
		pending := rplPages.test(id)
		// Partition (Inv-C2): a data page is exactly one of {reachable,
		// free, RPL-pending}. Any overlap is corruption.
		switch {
		case reach && free:
			if !c.emit(CheckIssue{Severity: CheckError, Code: "ReachableButFree", PageID: id,
				Message: fmt.Sprintf("page %d is referenced by the tree but marked free in the bitmap", id)}) {
				return
			}
		case reach && pending:
			if !c.emit(CheckIssue{Severity: CheckError, Code: "ReachableInRPL", PageID: id,
				Message: fmt.Sprintf("page %d is referenced by the tree but also pending RPL reclamation", id)}) {
				return
			}
		case !reach && free && pending:
			// A page on the free list AND in the RPL will be set free a
			// second time when reclamation processes its segment — a
			// future double-allocation hazard.
			if !c.emit(CheckIssue{Severity: CheckError, Code: "FreeAndPending", PageID: id,
				Message: fmt.Sprintf("page %d is both free in the bitmap and pending RPL reclamation", id)}) {
				return
			}
		case !reach && !free && !pending:
			if !c.emit(CheckIssue{Severity: CheckWarning, Code: "BitmapLeak", PageID: id,
				Message: fmt.Sprintf("page %d is allocated but unreferenced (leaked)", id)}) {
				return
			}
		}
	}
	// NumFree consistency (advisory under concurrent writes).
	if got := bm.Recount(); got != c.meta.NumFreePages {
		c.emit(CheckIssue{Severity: CheckWarning, Code: "FreeCountMismatch",
			Message: fmt.Sprintf("bitmap free-page count %d != meta NumFreePages %d", got, c.meta.NumFreePages)})
	}
}

// snapshotBitmap reconstructs the allocation bitmap from the snapshot's
// on-disk bitmap region (pages [2, 2+BitmapPages)). The detail bytes are
// copied out of the mmap so the returned bitmap is independent of any
// concurrent in-place bitmap pwrite.
func (c *checker) snapshotBitmap() (*bitmap.Bitmap, bool) {
	if c.meta.BitmapPages == 0 {
		return nil, false
	}
	ps := uint64(c.cfg.PageSize)
	detail := make([]byte, uint64(c.meta.BitmapPages)*ps)
	for i := uint64(0); i < uint64(c.meta.BitmapPages); i++ {
		copy(detail[i*ps:(i+1)*ps], c.rtx.pgr.Page(2+i))
	}
	return bitmap.New(detail, c.cfg.PageSize, c.meta.BitmapPages, c.meta.MaxSize), true
}

// bitset is a compact page-id membership set sized to a known upper
// bound (the snapshot HighWaterMark) — 1 bit per page, so 8 MB covers a
// 256 GB / 4 KB database, vs a map's per-entry overhead.
type bitset struct {
	words []uint64
	n     uint64
}

func newBitset(n uint64) bitset {
	return bitset{words: make([]uint64, (n+63)/64), n: n}
}

func (b bitset) set(id uint64) {
	if id < b.n {
		b.words[id>>6] |= 1 << (id & 63)
	}
}

// setIfInRange is set() but silently ignores ids outside [0, n) — used
// for RPL-listed page ids which a forged segment could push out of
// range (already reported elsewhere; here we just avoid OOB).
func (b bitset) setIfInRange(id uint64) { b.set(id) }

func (b bitset) test(id uint64) bool {
	if id >= b.n {
		return false
	}
	return b.words[id>>6]&(1<<(id&63)) != 0
}

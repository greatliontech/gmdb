package gmdb

import (
	"context"
	"errors"
	"fmt"
	"iter"

	"github.com/thegrumpylion/gmdb/internal/bitmap"
	"github.com/thegrumpylion/gmdb/internal/btree"
	"github.com/thegrumpylion/gmdb/internal/indexing"
	"github.com/thegrumpylion/gmdb/internal/lock"
	"github.com/thegrumpylion/gmdb/internal/page"
	"github.com/thegrumpylion/gmdb/internal/pager"
)

// rawPageReader feeds btree.Walk/WalkKV unverified bytes: Check reports
// corruption as CheckIssues rather than aborting the walk on it. The
// hwm bound inside Walk/WalkKV still prevents out-of-range reads.
type rawPageReader struct{ p *pager.Pager }

func (r rawPageReader) Page(id uint64) ([]byte, error) { return r.p.PageRaw(id), nil }

// verifyingPageReader is the conforming btree.PageReader (footer-verified
// on first access per the interface contract): a bad checksum yields
// ErrBadPageChecksum and aborts the walk. Used where source bytes are
// re-encoded (the compact rebuild in copy.go) and an unverified read would
// launder detectable bitrot into a fresh valid footer — the inverse of
// rawPageReader's report-don't-abort role in Check.
type verifyingPageReader struct{ p *pager.Pager }

func (r verifyingPageReader) Page(id uint64) ([]byte, error) { return r.p.Page(id) }

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

// CheckOptions configures CheckWithOptions. A nil *CheckOptions (or the
// zero value) is plain structural Check.
type CheckOptions struct {
	// Repair enables offline leaked-page reclamation: pages that are
	// allocated in the bitmap yet unreachable from every committed tree
	// and absent from the RPL (BitmapLeak) are freed in the bitmap.
	//
	// Repair requires EXCLUSIVE access (api-surface.md §CheckOptions):
	// it opens a WRITE transaction (acquiring the cross-process write
	// lock, so no concurrent writers) and proceeds only when no read
	// transaction is active in any process. With readers active it frees
	// nothing and emits a single CheckError "Repair.ReadersActive" — run
	// plain Check (no Repair) for read-only diagnostics in that case.
	//
	// Repair is conservative (api-surface.md §Check, CopyTo, Compact): it frees a page ONLY when the
	// structural walk completed without being stopped and emitted NO
	// CheckError/CheckFatal. Any structural finding makes the reachable
	// set unreliable, so a corrupt database reports its leaks with
	// Repaired=false plus a CheckWarning "Repair.Skipped" and reclaims
	// nothing. Reclaimed pages are reported as the usual BitmapLeak
	// CheckWarning with Repaired=true. The freed bitmap is published
	// through the normal commit pipeline (atomic meta-swap).
	Repair bool

	// CheckIndexes additionally verifies that each indexed keyspace's
	// stored index entries match what the supplied extractors would
	// produce — it re-runs every extractor over every row, O(rows ×
	// indexes). Off by default.
	//
	// When true, Indexes MUST carry an IndexDecl set for each indexed
	// keyspace to verify. An indexed keyspace absent from Indexes is
	// reported as a CheckWarning "CheckIndexes.KeyspaceNotSupplied" (its
	// structure is still checked). A drifted index is a CheckError
	// "CheckIndexes.FingerprintDrift" and does NOT abort the walk or
	// trigger a rebuild.
	CheckIndexes bool

	// Indexes supplies extractors for CheckIndexes, keyed by keyspace
	// name. Ignored when CheckIndexes is false. A keyspace name not in
	// the database is reported as "CheckIndexes.KeyspaceNotFound"; an
	// IndexDecl.Name not registered on an existing keyspace as
	// "CheckIndexes.IndexNotInRegistry".
	//
	// The supplied IndexDecl's Unique and Covering must match the
	// registered index's: the equivalence check reproduces the on-disk
	// (key, value) using the SUPPLIED decl, so a mismatched Unique or
	// Covering produces a FingerprintDrift (correctly — the supplied
	// decl does not describe the stored index). Both Keyspace and
	// SetKeyspace indexes are verified, each with its own codec.
	//
	// Beyond the four codes above, the pass may emit these diagnostic
	// codes (CheckError unless noted), all stable and prefixed
	// "CheckIndexes.": ExtractorMissing (supplied decl has a nil
	// Extract), ExtractorError (the extractor failed re-running, e.g. a
	// unique candidate-set collision), RowsUnreadable / IndexUnreadable
	// (CheckWarning — a corrupt tree blocked enumeration; the structural
	// pass already reported it), and KeyspaceKindUnsupported (CheckWarning
	// — a keyspace kind the pass cannot verify).
	Indexes map[string][]*IndexDecl
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
func (db *DB) Check() iter.Seq[CheckIssue] { return db.CheckWithOptions(nil) }

// CheckWithOptions is Check with options (api-surface.md §Check). With
// opts.CheckIndexes set it additionally verifies, for each supplied
// IndexDecl, that the stored index entries match what the extractor
// re-run over every live row would produce (extractor-equivalence). With
// opts.Repair set it reclaims leaked pages under exclusive access (see
// CheckOptions.Repair). A nil opts is identical to Check.
//
// The read-only modes (Repair unset) keep Check's reader-slot lifetime
// and CheckFatal-is-last contract. The Repair mode instead opens a WRITE
// transaction (exclusive access is required to free pages), but otherwise
// preserves the lazy-open and CheckFatal-is-last semantics.
func (db *DB) CheckWithOptions(opts *CheckOptions) iter.Seq[CheckIssue] {
	if opts != nil && opts.Repair {
		return db.checkRepair(opts)
	}
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
		meta := rtx.meta
		c := &checker{
			pgr:   rtx.pgr,
			cfg:   page.Config{PageSize: meta.PageSize, PageChecksum: meta.HasFlag(pager.MetaFlagPageChecksum)},
			meta:  meta,
			yield: yield,
			opts:  opts,
		}
		c.run()
	}
}

// checkRepair is the exclusive Repair path (CheckOptions.Repair). It
// opens a write transaction — acquiring the cross-process write lock, so
// no other writer runs concurrently — verifies no read transaction is
// active, runs the structural walk against the write tx's snapshot
// collecting the BitmapLeak set, and (only when the walk completed
// cleanly) frees exactly that set in the bitmap and commits. Repair conservatism (api-surface.md §Check, CopyTo, Compact):
// frees ONLY pages a COMPLETE, error-free walk proved unreachable, under
// verified no-readers/no-writers exclusivity; atomicity rides the commit
// pipeline.
func (db *DB) checkRepair(opts *CheckOptions) iter.Seq[CheckIssue] {
	return func(yield func(CheckIssue) bool) {
		tx, err := db.Begin(context.Background())
		if err != nil {
			yield(CheckIssue{
				Severity: CheckFatal,
				Code:     "Repair.WriteTxUnavailable",
				Message:  fmt.Sprintf("Repair could not open an exclusive write transaction: %v", err),
			})
			return
		}
		committed := false
		defer func() {
			if !committed {
				_ = tx.Rollback()
			}
		}()

		// Exclusivity gate (api-surface.md §Check, CopyTo, Compact): we hold the write
		// lock (no concurrent writers); require no live reader in any
		// process. OldestReaderTxnID's LOCK_EX precondition is satisfied
		// by the grant the write tx holds (same as db.Begin's bound
		// computation). Snapshot coord vs. a concurrent Close.
		coord := db.coordSnapshot()
		if coord == nil || coord.OldestReaderTxnID() != lock.NoReaderTxnID {
			yield(CheckIssue{
				Severity: CheckError,
				Code:     "Repair.ReadersActive",
				Message:  "Repair requires exclusive access but a read transaction is active; nothing reclaimed (run Check without Repair for read-only diagnostics)",
			})
			return
		}

		meta := tx.prevMeta
		c := &checker{
			pgr:    tx.pgr,
			cfg:    page.Config{PageSize: meta.PageSize, PageChecksum: meta.HasFlag(pager.MetaFlagPageChecksum)},
			meta:   meta,
			yield:  yield,
			opts:   opts,
			repair: true,
		}
		c.run()

		// Completeness gate (api-surface.md §Check, CopyTo, Compact): free only when the walk ran to
		// completion (caller did not break) and reported no structural
		// error/fatal. Otherwise the reachable set is unreliable and a
		// live page could be misclassified as leaked.
		if c.stopped {
			return // caller broke, or a CheckFatal already terminated the walk
		}
		if c.sawError {
			// Corruption present: report the leaks we found, unrepaired,
			// then a Skipped warning. Reclaim nothing.
			for _, id := range c.leaked {
				if !c.emitLeak(id, false) {
					return
				}
			}
			c.emit(CheckIssue{Severity: CheckWarning, Code: "Repair.Skipped",
				Message: "structural corruption present; leaked pages reported but not reclaimed (reachable set unreliable)"})
			return
		}
		if len(c.leaked) == 0 {
			return // structurally clean, no leaks — nothing to commit
		}

		// Free exactly the leaked set in the bitmap and publish via commit.
		for _, id := range c.leaked {
			if err := tx.pgr.FreeLeakedPage(id); err != nil {
				c.emit(CheckIssue{Severity: CheckFatal, Code: "Repair.FreeFailed", PageID: id,
					Message: fmt.Sprintf("could not free leaked page %d: %v", id, err)})
				return
			}
		}
		if err := tx.Commit(); err != nil {
			c.emit(CheckIssue{Severity: CheckFatal, Code: "Repair.CommitFailed",
				Message: fmt.Sprintf("repair commit failed; no pages reclaimed: %v", err)})
			return
		}
		committed = true
		for _, id := range c.leaked {
			if !c.emitLeak(id, true) {
				return
			}
		}
	}
}

// errCheckStop is returned by a checker's visit callback to abort an
// in-progress btree.Walk when the caller stopped iterating.
var errCheckStop = errors.New("check: iteration stopped by caller")

type checker struct {
	pgr   *pager.Pager
	cfg   page.Config
	meta  pager.Meta
	yield func(CheckIssue) bool
	opts  *CheckOptions

	stopped   bool
	reachable bitset

	// sawError latches true once any CheckError/CheckFatal is emitted —
	// the completeness gate keys Repair off it (a structurally
	// dirty walk leaves the reachable set unreliable, so Repair frees
	// nothing).
	sawError bool

	// repair, when set, makes accounting COLLECT the BitmapLeak set into
	// leaked rather than emit it inline; checkRepair frees the set and
	// emits each page (Repaired=true on success) after the walk.
	repair bool
	leaked []uint64

	// inv is the per-keyspace inventory the structural walk records for
	// the CheckIndexes pass — populated only when checkIndexesEnabled().
	inv map[string]*ksInventory
}

// ksInventory records, for the CheckIndexes pass, a keyspace's kind,
// data-tree root, the fixed value size (SetKeyspaces only), and the
// data-tree root of each registered index — gathered during the
// structural walk so the index pass needs no second descriptor read.
type ksInventory struct {
	kind           uint8  // keyspaceKind* — selects the index codec
	fixedValueSize uint16 // SetKeyspace subpage member width (0 ⇒ variable)
	dataRoot       uint64
	indexRoots     map[string]uint64 // index name → index data-tree root
}

func (c *checker) checkIndexesEnabled() bool { return c.opts != nil && c.opts.CheckIndexes }

// emit yields one issue. Returns false (and latches stopped) when the
// caller has asked to stop iterating. A CheckError/CheckFatal latches
// sawError (the completeness gate).
func (c *checker) emit(iss CheckIssue) bool {
	if c.stopped {
		return false
	}
	if iss.Severity >= CheckError {
		c.sawError = true
	}
	if !c.yield(iss) {
		c.stopped = true
		return false
	}
	return true
}

// emitLeak yields a BitmapLeak finding for page id, with Repaired set per
// whether the exclusive Repair path reclaimed it.
func (c *checker) emitLeak(id uint64, repaired bool) bool {
	msg := fmt.Sprintf("page %d is allocated but unreferenced (leaked)", id)
	if repaired {
		msg = fmt.Sprintf("page %d was allocated but unreferenced (leaked); reclaimed by Repair", id)
	}
	return c.emit(CheckIssue{Severity: CheckWarning, Code: "BitmapLeak", PageID: id, Repaired: repaired, Message: msg})
}

func (c *checker) run() {
	hwm := c.meta.HighWaterMark
	// Defend against a forged meta with HighWaterMark beyond what the
	// file actually covers: the reader mmap reservation is MaxSize pages
	// of ADDRESS SPACE but the FILE is only fileSize bytes, so a page id
	// in [filePages, MaxSize) would SIGBUS, and sizing the reachable
	// bitset to a forged-huge HWM/MaxSize would OOM. ValidateMeta does
	// not bound these, so clamp the walk to the real on-disk page count.
	// (Check never crashes on a forged page; integrity.md §Forged / structural corruption tolerance.)
	bound := min(uint64(c.pgr.FileSize())/uint64(c.cfg.PageSize), c.meta.MaxSize)
	if hwm > bound {
		c.emit(CheckIssue{Severity: CheckError, Code: "HighWaterMarkOutOfRange",
			Message: fmt.Sprintf("meta HighWaterMark %d exceeds file/MaxSize bound %d; walk clamped", hwm, bound)})
		hwm = bound
	}
	firstData := uint64(2) + uint64(c.meta.BitmapPages)
	c.reachable = newBitset(hwm)
	if c.checkIndexesEnabled() {
		c.inv = make(map[string]*ksInventory)
	}

	if err := pager.ValidateMeta(c.meta); err != nil {
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
	rplPages, ok := c.walkRPL(firstData, hwm)
	if !ok {
		return
	}

	c.accounting(firstData, hwm, rplPages)

	// Extractor-equivalence verification (opt-in). Runs after the
	// structural walk so the inventory is complete and the
	// CheckFatal-is-last contract holds (the structural/accounting passes
	// have already returned on any fatal; checkIndexes emits only
	// warnings + FingerprintDrift errors, never fatals).
	if c.checkIndexesEnabled() && !c.stopped {
		c.checkIndexes(hwm)
	}
}

// walkKeyspaces enumerates keyspaces from the snapshot's KeyspaceRoot,
// validates each descriptor, and walks each keyspace's data tree +
// index registry + index data trees into the reachable set. Enumeration
// uses the hwm-guarded btree.WalkKV (NOT the read cursor, whose descent
// is unguarded and would panic/SIGBUS on a corrupt or forged tree —
// Check must not crash on the corruption it exists to detect).
func (c *checker) walkKeyspaces(firstData, hwm uint64) bool {
	// The keyspace-descriptor tree itself is order-validated too — a
	// routing flip there makes OpenKeyspace descent miss a keyspace
	// mid-op while every per-page check stays clean.
	if _, _, ok, _ := c.validateTreeOrder("", "(keyspace tree)", c.meta.KeyspaceRoot, 0, hwm); !ok {
		return false
	}
	var keyspaceCount uint64
	err := btree.WalkKV(rawPageReader{c.pgr}, c.cfg, c.meta.KeyspaceRoot, hwm, func(k, v []byte) error {
		name := string(k)
		keyspaceCount++
		if len(v) != keyspaceDescriptorSize {
			if !c.emit(CheckIssue{Severity: CheckError, Code: "keyspaceDescriptorSize", Keyspace: name,
				Message: fmt.Sprintf("descriptor value length %d != %d", len(v), keyspaceDescriptorSize)}) {
				return errCheckStop
			}
			return nil
		}
		desc := decodeKeyspaceDescriptor(v)
		if verr := validateKeyspaceDescriptor(v, desc); verr != nil {
			if !c.emit(CheckIssue{Severity: CheckError, Code: "KeyspaceDescriptorInvalid", Keyspace: name,
				Message: verr.Error()}) {
				return errCheckStop
			}
			return nil
		}
		if c.checkIndexesEnabled() {
			c.inv[name] = &ksInventory{
				kind:           desc.Kind,
				fixedValueSize: desc.FixedValueSize,
				dataRoot:       desc.Root,
				indexRoots:     make(map[string]uint64),
			}
		}
		if !c.walkTree(name, "", desc.Root, firstData, hwm) {
			return errCheckStop
		}
		if entries, values, ok, structural := c.validateTreeOrder(name, "", desc.Root, desc.FixedValueSize, hwm); !ok {
			return errCheckStop
		} else if !structural {
			want, unit := entries, "entries"
			if desc.Kind == keyspaceKindSetKeyspace {
				want, unit = values, "values"
			}
			if desc.Count != want {
				if !c.emit(CheckIssue{Severity: CheckError, Code: "KeyspaceCountMismatch", Keyspace: name,
					Message: fmt.Sprintf("descriptor Count %d, tree holds %d %s", desc.Count, want, unit)}) {
					return errCheckStop
				}
			}
		}
		// SetKeyspace subpage payloads are INLINE in the outer-tree leaf
		// cells, not pages, so the page-level reachability walk above does
		// not see them. Validate them here so the structural Check honours
		// api-surface.md §Check's "verifies … set keyspace subpage …
		// integrity" claim (nested-tree integrity IS covered by the
		// reachability walk, which recurses nested subtrees).
		if desc.Kind == keyspaceKindSetKeyspace {
			if !c.checkSetKeyspaceSubpages(name, desc.Root, desc.FixedValueSize, hwm) {
				return errCheckStop
			}
		}
		if desc.IndexRegistryRoot != 0 {
			if !c.walkRegistry(name, desc, firstData, hwm) {
				return errCheckStop
			}
		}
		return nil
	})
	if err == nil && keyspaceCount != c.meta.NumKeyspaces {
		if !c.emit(CheckIssue{Severity: CheckError, Code: "NumKeyspacesMismatch",
			Message: fmt.Sprintf("meta.NumKeyspaces %d, descriptor tree holds %d", c.meta.NumKeyspaces, keyspaceCount)}) {
			return false
		}
	}
	return c.dispositionEnumErr(err, "KeyspaceWalkFailed", "", "keyspace enumeration")
}

// validateTreeOrder runs the tree-level ordering/consistency pass
// (api-surface.md §Check: key ordering, separator routing,
// nested-tree member counts, descriptor counts) that per-page
// checksums and structural Validate cannot see. One extra read pass
// over the keyspace's live pages; violations are CheckError. A
// structural walk failure is NOT re-reported here — walkTree already
// ran on this root.
func (c *checker) validateTreeOrder(ks, idx string, root uint64, fvs uint16, hwm uint64) (entries, values uint64, ok, structural bool) {
	stopped := false
	entries, values, err := btree.ValidateOrder(rawPageReader{c.pgr}, c.cfg, root, hwm, fvs,
		func(kind btree.OrderViolationKind, pageID uint64, msg string) bool {
			code := "KeyOrderViolation"
			if kind == btree.OrderNestedCount {
				code = "NestedCountMismatch"
			}
			if !c.emit(CheckIssue{Severity: CheckError, Code: code, PageID: pageID, Keyspace: ks, Index: idx, Message: msg}) {
				stopped = true
				return false
			}
			return true
		})
	if stopped {
		return 0, 0, false, false
	}
	if err != nil {
		return 0, 0, true, true // structural failure already reported by walkTree
	}
	return entries, values, true, false
}

// checkSetKeyspaceSubpages validates every SetKeyspace subpage cell's
// internal header (a forged Count / DataSize, or a value shorter than the
// subpage header) — corruption the page-level reachability walk cannot
// see, since subpages are inline in the outer-tree leaf cells. Enumerates
// via the guarded WalkLeafEntries (never panics on a forged tree) and
// constructs the reader with the keyspace's FixedValueSize so fixed- and
// variable-width subpages are both validated faithfully. A bad subpage is
// a SubpageCorrupt CheckError. A WalkLeafEntries structural failure is NOT
// re-reported here — the reachability walkTree pass already ran and
// reported the tree-structure corruption. Returns false only when the
// caller stopped iterating.
func (c *checker) checkSetKeyspaceSubpages(ks string, dataRoot uint64, fvs uint16, hwm uint64) bool {
	if dataRoot == 0 {
		return true
	}
	err := btree.WalkLeafEntries(rawPageReader{c.pgr}, c.cfg, dataRoot, hwm, func(e page.LeafEntry) error {
		if !e.IsSubpage() {
			return nil // nested-tree / other cells are covered by the reachability walk
		}
		// NewSubpageReader panics below SubpageHeaderSize and Validate is
		// not total over a malformed header — bound the length first, the
		// same guard the CheckIndexes path uses.
		if len(e.Value) < page.SubpageHeaderSize {
			if !c.emit(CheckIssue{Severity: CheckError, Code: "SubpageCorrupt", Keyspace: ks,
				Message: fmt.Sprintf("set key %q subpage is %d bytes (< header %d)", e.Key, len(e.Value), page.SubpageHeaderSize)}) {
				return errCheckStop
			}
			return nil
		}
		if verr := page.NewSubpageReader(e.Value, fvs).Validate(); verr != nil {
			if !c.emit(CheckIssue{Severity: CheckError, Code: "SubpageCorrupt", Keyspace: ks,
				Message: fmt.Sprintf("set key %q subpage invalid: %v", e.Key, verr)}) {
				return errCheckStop
			}
		}
		return nil
	})
	if err != nil && errors.Is(err, errCheckStop) {
		return false
	}
	// A non-stop WalkLeafEntries error is tree-structure corruption already
	// reported by the reachability walkTree pass — do not double-report.
	return true
}

// walkRegistry walks a keyspace's index registry sub-tree (registry
// pages) and, for each registry entry, the index's data tree. Uses the
// guarded WalkKV for the same reason as walkKeyspaces.
func (c *checker) walkRegistry(ks string, desc keyspaceDescriptor, firstData, hwm uint64) bool {
	if !c.walkTree(ks, "", desc.IndexRegistryRoot, firstData, hwm) {
		return false
	}
	if _, _, ok, _ := c.validateTreeOrder(ks, "(index registry)", desc.IndexRegistryRoot, 0, hwm); !ok {
		return false
	}
	err := btree.WalkKV(rawPageReader{c.pgr}, c.cfg, desc.IndexRegistryRoot, hwm, func(k, v []byte) error {
		idxName := string(k)
		entry, derr := indexing.DecodeRegistryEntry(v)
		if derr != nil {
			if !c.emit(CheckIssue{Severity: CheckError, Code: "RegistryEntryInvalid", Keyspace: ks, Index: idxName,
				Message: fmt.Sprintf("registry entry decode: %v", derr)}) {
				return errCheckStop
			}
			return nil
		}
		if c.checkIndexesEnabled() {
			if info := c.inv[ks]; info != nil {
				info.indexRoots[idxName] = entry.Root
			}
		}
		if !c.walkTree(ks, idxName, entry.Root, firstData, hwm) {
			return errCheckStop
		}
		if _, _, ok, _ := c.validateTreeOrder(ks, idxName, entry.Root, 0, hwm); !ok {
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
			if !page.VerifyPageFooter(c.pgr.PageRaw(id), c.cfg.PageSize) {
				if !c.emit(CheckIssue{Severity: CheckError, Code: "BadPageChecksum", PageID: id, Keyspace: ks, Index: idx,
					Message: fmt.Sprintf("page %d checksum mismatch", id)}) {
					return errCheckStop
				}
			}
		}
		return nil
	}
	err := btree.Walk(rawPageReader{c.pgr}, c.cfg, root, hwm, visit)
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

// walkRPL walks the snapshot's RPL chain head→tail through the shared
// pager.RPLChainWalk convention (the same implementation the Open-time
// rebuild uses — free-space.md §RPL requires the two consumers to
// agree), returning the set of pages pending reclamation (the segment
// pages themselves + every entry they list). Truncation boundaries
// (stale tail on a non-latest meta) end the walk silently except the
// checksum boundary, which warns: a checksum-bad-but-decodable segment
// would otherwise pass Check clean while reclamation quarantines it.
// Hard walk errors surface as per-kind CheckError issues. firstData is
// run()'s first-data-page boundary (the meta/bitmap region ends there).
// Returns ok=false only when the caller stopped iterating.
func (c *checker) walkRPL(firstData, hwm uint64) (bitset, bool) {
	pending := newBitset(hwm)
	head := c.meta.RPLHeadPage
	if head == 0 {
		return pending, true
	}
	// bm is the snapshot's allocation bitmap — the reclaimed-segment
	// oracle; if unavailable the walk falls back to the footer/decode
	// boundary alone.
	bm, bmOK := c.snapshotBitmap()
	walk := pager.RPLChainWalk{
		ReadPage:     c.pgr.PageRaw,
		Cfg:          c.cfg,
		Head:         head,
		HeadTxnID:    c.meta.RPLHeadTxnID,
		Tail:         c.meta.RPLTailPage,
		EntryCount:   c.meta.RPLEntryCount,
		ReclaimEpoch: c.meta.Durable.TxnID,
		LowBound:     firstData,
		HighBound:    hwm,
		IsFree: func(id uint64) (bool, bool) {
			if !bmOK {
				return false, false
			}
			return bm.IsSet(id), true
		},
	}
	stop, werr := walk.Walk(func(id uint64, seg pager.RPLSegment) bool {
		pending.setIfInRange(id)
		for _, pid := range seg.PageIDs {
			pending.setIfInRange(pid)
		}
		return true
	})
	if werr != nil {
		return pending, c.emit(rplWalkIssue(werr, hwm))
	}
	if stop.Reason == pager.RPLWalkFooterBoundary {
		if !c.emit(CheckIssue{Severity: CheckWarning, Code: "RPLSegmentChecksum", PageID: stop.PageID,
			Message: fmt.Sprintf("RPL segment page %d fails checksum; chain walk stopped before tail %d (pages behind the boundary surface as BitmapLeak until reclamation quarantines the segment)", stop.PageID, c.meta.RPLTailPage)}) {
			return pending, false
		}
	}
	return pending, true
}

// rplWalkIssue maps a hard chain-walk failure to Check's stable issue
// codes. hwm is walkRPL's walk bound (the clamped HighWaterMark), named
// in the out-of-range message.
func rplWalkIssue(werr *pager.RPLWalkError, hwm uint64) CheckIssue {
	iss := CheckIssue{Severity: CheckError, PageID: werr.PageID}
	switch werr.Kind {
	case pager.RPLWalkErrTailMissing:
		iss.Code = "RPLTailMissing"
		iss.Message = fmt.Sprintf("RPL head %d set but tail page is 0", werr.Head)
	case pager.RPLWalkErrSegmentOutOfRange:
		iss.Code = "RPLSegmentOutOfRange"
		iss.Message = fmt.Sprintf("RPL segment page %d >= HighWaterMark %d", werr.PageID, hwm)
	case pager.RPLWalkErrSegmentInMetaRegion:
		iss.Code = "RPLSegmentInMetaRegion"
		iss.Message = fmt.Sprintf("RPL segment page %d inside the meta/bitmap region (first data page %d)", werr.PageID, werr.Bound)
	case pager.RPLWalkErrChainCycle:
		iss.Code = "RPLChainCycle"
		iss.Message = fmt.Sprintf("RPL chain cycle at segment page %d", werr.PageID)
	case pager.RPLWalkErrChainTooLong:
		iss.Code = "RPLChainTooLong"
		iss.Message = fmt.Sprintf("RPL chain exceeds entry-count bound %d (likely cycle)", werr.Bound)
	case pager.RPLWalkErrHeadChecksum:
		iss.Code = "RPLSegmentChecksum"
		iss.Message = fmt.Sprintf("RPL head segment page %d fails checksum", werr.PageID)
	case pager.RPLWalkErrHeadMalformed:
		iss.Code = "RPLSegmentMalformed"
		iss.Message = fmt.Sprintf("RPL head segment page %d malformed", werr.PageID)
	case pager.RPLWalkErrEndedBeforeTail:
		iss.Code = "RPLChainEndedBeforeTail"
		iss.Message = fmt.Sprintf("RPL chain from head %d ended before tail %d", werr.Head, werr.Tail)
	default:
		iss.Code = "RPLChainWalkFailed"
		iss.Message = werr.Error()
	}
	return iss
}

// accounting compares the reachable + RPL-pending sets against the
// snapshot's allocation bitmap for every data page in [firstData, hwm):
// a reachable page the bitmap marks free is a ReachableButFree error; an
// allocated page that is neither reachable nor RPL-pending is a
// BitmapLeak warning. (page-accounting partition; api-surface.md §Check, CopyTo, Compact.)
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
		// Partition (api-surface.md §Check, CopyTo, Compact): a data page is exactly one of {reachable,
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
			if c.repair {
				// Defer emission: checkRepair frees these after the walk
				// (Repair needs the COMPLETE reachable set first) and emits
				// each with Repaired set per the outcome.
				c.leaked = append(c.leaked, id)
			} else if !c.emitLeak(id, false) {
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

// checkIndexes runs the extractor-equivalence pass (CheckOptions.
// CheckIndexes). For each indexed keyspace not covered by opts.Indexes it
// emits KeyspaceNotSupplied; for each supplied (keyspace, IndexDecl) it
// flags KeyspaceNotFound / IndexNotInRegistry on misconfiguration, else
// verifies the stored index equals what the extractor re-run over every
// row produces. Read-only; emits only warnings + FingerprintDrift errors,
// never CheckFatal, so the CheckFatal-is-last contract is preserved. Map
// iteration order is unspecified, but findings are a set so order is
// immaterial.
func (c *checker) checkIndexes(hwm uint64) {
	// Indexed keyspaces the caller supplied no extractors for.
	for ksName, info := range c.inv {
		if len(info.indexRoots) == 0 {
			continue // not an indexed keyspace
		}
		if _, ok := c.opts.Indexes[ksName]; ok {
			continue
		}
		if !c.emit(CheckIssue{Severity: CheckWarning, Code: "CheckIndexes.KeyspaceNotSupplied", Keyspace: ksName,
			Message: fmt.Sprintf("indexed keyspace %q has no IndexDecls supplied; extractor-equivalence skipped (structure still checked)", ksName)}) {
			return
		}
	}
	// Supplied extractors → verify, or flag misconfiguration.
	for ksName, decls := range c.opts.Indexes {
		info, ok := c.inv[ksName]
		if !ok {
			if !c.emit(CheckIssue{Severity: CheckWarning, Code: "CheckIndexes.KeyspaceNotFound", Keyspace: ksName,
				Message: fmt.Sprintf("supplied keyspace %q does not exist in the database", ksName)}) {
				return
			}
			continue
		}
		for _, decl := range decls {
			if decl == nil {
				continue
			}
			idxRoot, ok := info.indexRoots[decl.Name]
			if !ok {
				if !c.emit(CheckIssue{Severity: CheckWarning, Code: "CheckIndexes.IndexNotInRegistry", Keyspace: ksName, Index: decl.Name,
					Message: fmt.Sprintf("supplied index %q is not registered on keyspace %q", decl.Name, ksName)}) {
					return
				}
				continue
			}
			if !c.verifyIndexEquivalence(ksName, info, decl, idxRoot, hwm) {
				return
			}
		}
	}
}

// verifyIndexEquivalence re-runs decl.Extract over every row of the
// keyspace data tree, accumulates the encoded index keys the extractor
// would produce, and compares that set against the keys stored in the
// index data tree. A discrepancy is a FingerprintDrift CheckError (the
// index does not match the supplied extractor — typically an extractor
// changed without a Version bump, indexing.md §Drift Guard). Both walks
// use the guarded WalkKV (no panic on a forged tree). Returns false only
// when the caller stopped iterating.
func (c *checker) verifyIndexEquivalence(ks string, info *ksInventory, decl *IndexDecl, idxRoot, hwm uint64) bool {
	if decl.Extract == nil {
		return c.emit(CheckIssue{Severity: CheckError, Code: "CheckIndexes.ExtractorMissing", Keyspace: ks, Index: decl.Name,
			Message: fmt.Sprintf("supplied IndexDecl %q has a nil Extract", decl.Name)})
	}
	hasCovering := len(decl.Covering) > 0
	// Build the (encoded key → encoded value) set the index SHOULD contain
	// by re-running the extractor over the live rows/members, reproducing
	// the exact on-disk (key, value) the write path stores — using the
	// codec for this keyspace kind. extractErr = the extractor itself
	// failed; structErr = enumerating rows/members hit a corrupt page
	// (already reported by the structural pass).
	var (
		expected   map[string]string
		extractErr error
		structErr  error
	)
	switch info.kind {
	case keyspaceKindKeyspace:
		expected, extractErr, structErr = c.expectedKeyspaceIndex(decl, info.dataRoot, hwm, hasCovering)
	case keyspaceKindSetKeyspace:
		expected, extractErr, structErr = c.expectedSetKeyspaceIndex(decl, info.dataRoot, info.fixedValueSize, hwm, hasCovering)
	default:
		return c.emit(CheckIssue{Severity: CheckWarning, Code: "CheckIndexes.KeyspaceKindUnsupported", Keyspace: ks, Index: decl.Name,
			Message: fmt.Sprintf("keyspace %q has kind %d, which CheckIndexes cannot verify", ks, info.kind)})
	}
	if extractErr != nil {
		return c.emit(CheckIssue{Severity: CheckError, Code: "CheckIndexes.ExtractorError", Keyspace: ks, Index: decl.Name,
			Message: fmt.Sprintf("re-running extractor failed: %v", extractErr)})
	}
	if structErr != nil {
		// Structural failure enumerating rows/members (already reported by
		// the structural pass); skip equivalence for this index.
		return c.emit(CheckIssue{Severity: CheckWarning, Code: "CheckIndexes.RowsUnreadable", Keyspace: ks, Index: decl.Name,
			Message: fmt.Sprintf("could not enumerate rows/members for index verification: %v", structErr)})
	}
	// Stored entries: enumerate the index data tree.
	stored := make(map[string]string)
	serr := btree.WalkKV(rawPageReader{c.pgr}, c.cfg, idxRoot, hwm, func(k, v []byte) error {
		stored[string(k)] = string(v)
		return nil
	})
	if serr != nil {
		return c.emit(CheckIssue{Severity: CheckWarning, Code: "CheckIndexes.IndexUnreadable", Keyspace: ks, Index: decl.Name,
			Message: fmt.Sprintf("could not enumerate stored index entries: %v", serr)})
	}
	if missing, extra, mism := diffEntrySets(expected, stored); missing > 0 || extra > 0 || mism > 0 {
		return c.emit(CheckIssue{Severity: CheckError, Code: "CheckIndexes.FingerprintDrift", Keyspace: ks, Index: decl.Name,
			Message: fmt.Sprintf("index %q drift: %d expected entries missing from the index, %d stored entries the extractor did not produce, %d value mismatches",
				decl.Name, missing, extra, mism)})
	}
	return true
}

// expectedKeyspaceIndex re-runs decl.Extract over every row of a plain
// Keyspace's data tree and returns the (encoded key → encoded value) set
// the index should hold, using the Keyspace codec (row key as PK).
func (c *checker) expectedKeyspaceIndex(decl *IndexDecl, dataRoot, hwm uint64, hasCovering bool) (expected map[string]string, extractErr, structErr error) {
	expected = make(map[string]string)
	werr := btree.WalkKV(rawPageReader{c.pgr}, c.cfg, dataRoot, hwm, func(k, v []byte) error {
		entries, eerr := extractEntriesAsKeySet(decl, k, v)
		if eerr != nil {
			extractErr = eerr
			return errCheckStop
		}
		for ek, entry := range entries {
			expected[ek] = string(indexEntryValue(entry, k, decl.Unique, hasCovering))
		}
		return nil
	})
	if extractErr != nil {
		return nil, extractErr, nil
	}
	if werr != nil {
		return nil, nil, werr
	}
	return expected, nil, nil
}

// expectedSetKeyspaceIndex re-runs decl.Extract over every (setKey,
// member) pair of a SetKeyspace and returns the expected (encoded key →
// encoded value) set, using the SetKeyspace codec (encodeSetKeyspaceIndexKey
// + the compound (setKey,member) PK). Members live in a subpage (small
// sets) or a nested B+tree keyed by the set key in the outer tree; both
// are enumerated through the guarded walkers so a forged tree yields an
// error, not a panic.
func (c *checker) expectedSetKeyspaceIndex(decl *IndexDecl, dataRoot uint64, fvs uint16, hwm uint64, hasCovering bool) (expected map[string]string, extractErr, structErr error) {
	expected = make(map[string]string)
	pr := rawPageReader{c.pgr}
	addMember := func(setKey, member []byte) error {
		entries, eerr := setKeyspaceExtractEntries(decl, setKey, member)
		if eerr != nil {
			extractErr = eerr
			return errCheckStop
		}
		compoundPK := encodeSetKeyspaceCompoundPK(setKey, member)
		for ek, entry := range entries {
			expected[ek] = string(indexEntryValue(entry, compoundPK, decl.Unique, hasCovering))
		}
		return nil
	}
	werr := btree.WalkLeafEntries(pr, c.cfg, dataRoot, hwm, func(e page.LeafEntry) error {
		switch {
		case e.IsSubpage():
			// Guard the raw on-disk subpage before decoding it — a forged
			// subpage (too short, or a bad internal Count/DataSize) must
			// surface as ErrCorrupted (→ a RowsUnreadable warning), never
			// a panic: NewSubpageReader panics below SubpageHeaderSize and
			// AllValues is not total over a malformed header. This upholds
			// the "Check never panics on a forged page" contract.
			if len(e.Value) < page.SubpageHeaderSize {
				return fmt.Errorf("%w: SetKeyspace subpage for key %q is %d bytes (< header %d)",
					btree.ErrCorrupted, e.Key, len(e.Value), page.SubpageHeaderSize)
			}
			sp := page.NewSubpageReader(e.Value, fvs)
			if err := sp.Validate(); err != nil {
				return fmt.Errorf("%w: SetKeyspace subpage for key %q: %w", btree.ErrCorrupted, e.Key, err)
			}
			var inErr error
			sp.AllValues(func(member []byte) bool {
				if err := addMember(e.Key, member); err != nil {
					inErr = err
					return false
				}
				return true
			})
			return inErr
		case e.IsNestedTree():
			// Members are the KEYS of the nested tree (empty values), per
			// set-keyspace.md §Storage Strategy.
			return btree.WalkKV(pr, c.cfg, e.NestedRoot, hwm, func(member, _ []byte) error {
				return addMember(e.Key, member)
			})
		default:
			return fmt.Errorf("%w: SetKeyspace entry for key %q is neither subpage nor nested-tree (flags 0x%x)",
				btree.ErrCorrupted, e.Key, e.Flags)
		}
	})
	if extractErr != nil {
		return nil, extractErr, nil
	}
	if werr != nil {
		return nil, nil, werr
	}
	return expected, nil, nil
}

// diffEntrySets compares two (encoded key → encoded value) maps: missing
// = a key in expected but absent from stored; extra = a key in stored but
// absent from expected; mismatch = a key in both whose values differ.
func diffEntrySets(expected, stored map[string]string) (missing, extra, mismatch int) {
	for k, ev := range expected {
		sv, ok := stored[k]
		if !ok {
			missing++
			continue
		}
		if sv != ev {
			mismatch++
		}
	}
	for k := range stored {
		if _, ok := expected[k]; !ok {
			extra++
		}
	}
	return missing, extra, mismatch
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
		copy(detail[i*ps:(i+1)*ps], c.pgr.PageRaw(2+i))
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

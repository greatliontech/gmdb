// Package verify holds the structural-integrity checker behind
// DB.Check / CheckWithOptions and the read-only tree page-stats
// walker: it walks a pager snapshot (Checker's exported fields are
// the verifier input — pager, page config, meta, yield, options)
// and emits Issues with the stable machine-parseable Codes the
// api-surface spec pins. The transaction/repair drivers live with
// the engine.
package verify

import (
	"errors"
	"fmt"

	"github.com/greatliontech/gmdb/internal/bitmap"
	"github.com/greatliontech/gmdb/internal/btree"
	"github.com/greatliontech/gmdb/internal/descriptor"
	"github.com/greatliontech/gmdb/internal/indexing"
	"github.com/greatliontech/gmdb/internal/page"
	"github.com/greatliontech/gmdb/internal/pager"
)

// Severity grades an Issue.
type Severity int

const (
	// Warning is a non-critical finding (e.g. a leaked-but-harmless
	// page, a free-count mismatch under concurrent writes).
	Warning Severity = iota
	// Error is a structural integrity violation (bad checksum,
	// malformed page, a reachable page the bitmap marks free).
	Error
	// Fatal marks a point past which the walk could not continue;
	// it is always the last issue yielded.
	Fatal
)

// Issue is one finding from a Check walk. See api-surface.md
// §Check, CopyTo, Compact. Code is a stable machine-parseable token;
// Message is free-form human-facing text (do not pattern-match on it).
type Issue struct {
	Severity Severity
	Code     string
	PageID   uint64
	Keyspace string
	Index    string
	Message  string
	Repaired bool
}

// Options configures CheckWithOptions. A nil *Options (or the
// zero value) is plain structural Check.
type Options struct {
	// Repair enables offline leaked-page reclamation: pages that are
	// allocated in the bitmap yet unreachable from every committed tree
	// and absent from the RPL (BitmapLeak) are freed in the bitmap.
	//
	// Repair requires EXCLUSIVE access (api-surface.md §CheckOptions):
	// it opens a WRITE transaction (acquiring the cross-process write
	// lock, so no concurrent writers) and proceeds only when no read
	// transaction is active in any process. With readers active it frees
	// nothing and emits a single Error "Repair.ReadersActive" — run
	// plain Check (no Repair) for read-only diagnostics in that case.
	//
	// Repair is conservative (api-surface.md §Check, CopyTo, Compact): it frees a page ONLY when the
	// structural walk completed without being stopped, emitted NO
	// Error/Fatal, AND the RPL chain walk reached its
	// authoritative tail or a reclaimed boundary. A structural finding
	// makes the reachable set unreliable, and an RPL walk truncated at
	// a corrupt-segment boundary hides still-pending segments whose
	// entries then misclassify as leaked — either way the database
	// reports its leaks with Repaired=false plus a Warning
	// "Repair.Skipped" and reclaims nothing. Reclaimed pages are
	// reported as the usual BitmapLeak Warning with Repaired=true. The freed bitmap is published
	// through the normal commit pipeline (atomic meta-swap).
	Repair bool

	// CheckIndexes additionally verifies that each indexed keyspace's
	// stored index entries match what the supplied extractors would
	// produce — it re-runs every extractor over every row, O(rows ×
	// indexes). Off by default.
	//
	// When true, Indexes MUST carry an indexing.Decl set for each indexed
	// keyspace to verify. An indexed keyspace absent from Indexes is
	// reported as a Warning "CheckIndexes.KeyspaceNotSupplied" (its
	// structure is still checked). A drifted index is an Error
	// "CheckIndexes.FingerprintDrift" and does NOT abort the walk or
	// trigger a rebuild.
	CheckIndexes bool

	// Indexes supplies extractors for CheckIndexes, keyed by keyspace
	// name. Ignored when CheckIndexes is false. A keyspace name not in
	// the database is reported as "CheckIndexes.KeyspaceNotFound"; an
	// indexing.Decl.Name not registered on an existing keyspace as
	// "CheckIndexes.IndexNotInRegistry".
	//
	// The supplied indexing.Decl's Unique and Covering must match the
	// registered index's: the equivalence check reproduces the on-disk
	// (key, value) using the SUPPLIED decl, so a mismatched Unique or
	// Covering produces a FingerprintDrift (correctly — the supplied
	// decl does not describe the stored index). Both Keyspace and
	// SetKeyspace indexes are verified, each with its own codec.
	//
	// Beyond the four codes above, the pass may emit these diagnostic
	// codes (Error unless noted), all stable and prefixed
	// "CheckIndexes.": ExtractorMissing (supplied decl has a nil
	// Extract), ExtractorError (the extractor failed re-running, e.g. a
	// unique candidate-set collision), RowsUnreadable / IndexUnreadable
	// (Warning — a corrupt tree blocked enumeration; the structural
	// pass already reported it), and KeyspaceKindUnsupported (Warning
	// — a keyspace kind the pass cannot verify).
	Indexes map[string][]*indexing.Decl
}

// RawPageReader feeds btree.Walk/WalkKV unverified bytes: Check reports
// corruption as Issues rather than aborting the walk on it. The
// hwm bound inside Walk/WalkKV still prevents out-of-range reads.
type RawPageReader struct{ P *pager.Pager }

func (r RawPageReader) Page(id uint64) ([]byte, error) { return r.P.PageRaw(id), nil }

// VerifyingPageReader is the conforming btree.PageReader (footer-verified
// on first access per the interface contract): a bad checksum yields
// ErrBadPageChecksum and aborts the walk. Used where source bytes are
// re-encoded (the compact rebuild in copy.go) and an unverified read would
// launder detectable bitrot into a fresh valid footer — the inverse of
// RawPageReader's report-don't-abort role in Check.
type VerifyingPageReader struct{ P *pager.Pager }

func (r VerifyingPageReader) Page(id uint64) ([]byte, error) { return r.P.Page(id) }

// errCheckStop is returned by a Checker's visit callback to abort an
// in-progress btree.Walk when the caller stopped iterating.
var errCheckStop = errors.New("check: iteration stopped by caller")

type Checker struct {
	Pgr   *pager.Pager
	Cfg   page.Config
	Meta  pager.Meta
	Yield func(Issue) bool
	Opts  *Options

	Stopped   bool
	reachable Bitset

	// sawError latches true once any Error/Fatal is emitted —
	// the completeness gate keys Repair off it (a structurally
	// dirty walk leaves the reachable set unreliable, so Repair frees
	// nothing).
	SawError bool

	// rplBoundary latches true when the RPL chain walk stopped at a
	// footer/decode boundary — an AMBIGUOUS truncation (a segment can
	// bitrot after the live writer built its in-memory chain, so the
	// behind-boundary segments may still be live pending state whose
	// entries then misclassify as leaked). Reclamation — background
	// maintenance and Repair alike — must free nothing while it is
	// set. A reclaimed boundary does NOT latch it: when every live
	// handle's chain derives from a walk over the current image
	// (crash recovery at Open, Resync after a TxnID advance), all
	// walkers truncate there identically and behind-boundary pages
	// are genuinely free. The residual — a surviving handle whose
	// chain predates a peer's TORN, never-published reclamation
	// (step-1 bitmap pwrites without the meta) — is recorded in
	// background-maintenance.md §Bitmap Leak Reclamation.
	RPLBoundary bool

	// repair, when set, makes accounting COLLECT the BitmapLeak set into
	// leaked rather than emit it inline; checkRepair frees the set and
	// emits each page (Repaired=true on success) after the walk.
	Repair bool
	Leaked []uint64

	// ExtractKeySet / SetExtractKeySet run a supplied decl's
	// extractor over one row / set member and return the encoded
	// key-set — the engine's maintenance-path glue, injected so the
	// CheckIndexes pass reproduces entries EXACTLY as live
	// maintenance would (same candidate-set collision errors, same
	// sentinels). Part of the verifier input: both MUST be non-nil
	// whenever Opts.CheckIndexes is set (Run does not guard — a nil
	// func panics at first use); Checkers that never enable
	// CheckIndexes may leave them nil.
	ExtractKeySet    func(decl *indexing.Decl, key, value []byte) (map[string]indexing.Entry, error)
	SetExtractKeySet func(decl *indexing.Decl, setKey, setValue []byte) (map[string]indexing.Entry, error)

	// inv is the per-keyspace inventory the structural walk records for
	// the CheckIndexes pass — populated only when checkIndexesEnabled().
	inv map[string]*ksInventory
}

// ksInventory records, for the CheckIndexes pass, a keyspace's kind,
// data-tree root, the fixed value size (SetKeyspaces only), and the
// data-tree root of each registered index — gathered during the
// structural walk so the index pass needs no second descriptor read.
type ksInventory struct {
	kind           uint8  // descriptor.Kind* — selects the index codec
	fixedValueSize uint16 // SetKeyspace subpage member width (0 ⇒ variable)
	dataRoot       uint64
	indexRoots     map[string]uint64 // index name → index data-tree root
}

func (c *Checker) checkIndexesEnabled() bool { return c.Opts != nil && c.Opts.CheckIndexes }

// Emit yields one issue. Returns false (and latches stopped) when the
// caller has asked to stop iterating. An Error/Fatal latches
// sawError (the completeness gate).
func (c *Checker) Emit(iss Issue) bool {
	if c.Stopped {
		return false
	}
	if iss.Severity >= Error {
		c.SawError = true
	}
	if !c.Yield(iss) {
		c.Stopped = true
		return false
	}
	return true
}

// EmitLeak yields a BitmapLeak finding for page id, with Repaired set per
// whether the exclusive Repair path reclaimed it.
func (c *Checker) EmitLeak(id uint64, repaired bool) bool {
	msg := fmt.Sprintf("page %d is allocated but unreferenced (leaked)", id)
	if repaired {
		msg = fmt.Sprintf("page %d was allocated but unreferenced (leaked); reclaimed by Repair", id)
	}
	return c.Emit(Issue{Severity: Warning, Code: CodeBitmapLeak, PageID: id, Repaired: repaired, Message: msg})
}

func (c *Checker) Run() {
	hwm := c.Meta.HighWaterMark
	// Defend against a forged meta with HighWaterMark beyond what the
	// file actually covers: the reader mmap reservation is MaxSize pages
	// of ADDRESS SPACE but the FILE is only fileSize bytes, so a page id
	// in [filePages, MaxSize) would SIGBUS, and sizing the reachable
	// Bitset to a forged-huge HWM/MaxSize would OOM. ValidateMeta does
	// not bound these, so clamp the walk to the real on-disk page count.
	// (Check never crashes on a forged page; integrity.md §Forged / structural corruption tolerance.)
	bound := min(uint64(c.Pgr.FileSize())/uint64(c.Cfg.PageSize), c.Meta.MaxSize)
	if hwm > bound {
		c.Emit(Issue{Severity: Error, Code: CodeHighWaterMarkOutOfRange,
			Message: fmt.Sprintf("meta HighWaterMark %d exceeds file/MaxSize bound %d; walk clamped", hwm, bound)})
		hwm = bound
	}
	firstData := uint64(2) + uint64(c.Meta.BitmapPages)
	c.reachable = NewBitset(hwm)
	if c.checkIndexesEnabled() {
		c.inv = make(map[string]*ksInventory)
	}

	if err := pager.ValidateMeta(c.Meta); err != nil {
		if !c.Emit(Issue{Severity: Error, Code: CodeMetaInvalid,
			Message: fmt.Sprintf("active meta invalid: %v", err)}) {
			return
		}
	}

	// Walk the top-level keyspace B+tree, then every keyspace's data
	// tree, index registry, and index data trees.
	if !c.walkTree("", "", c.Meta.KeyspaceRoot, firstData, hwm) {
		return
	}
	if c.Meta.KeyspaceRoot != 0 {
		if !c.walkKeyspaces(firstData, hwm) {
			return
		}
	}

	// ONE bitmap copy shared by the RPL walk's reclaimed-boundary
	// oracle and the accounting partition: the live bitmap has no MVCC
	// (commits pwrite it in place), so two copies taken at different
	// instants can disagree — the pending set built against the first
	// and the free set read from the second would then emit spurious
	// FreeAndPending/ReachableButFree Errors on a healthy database
	// under a concurrent writer. A single copy makes that incoherence
	// unrepresentable.
	bm, bmOK := c.SnapshotBitmap()

	// RPL chain → set of pages pending reclamation.
	rplPages, ok := c.walkRPL(firstData, hwm, bm, bmOK)
	if !ok {
		return
	}

	c.accounting(firstData, hwm, rplPages, bm, bmOK)

	// Extractor-equivalence verification (opt-in). Runs after the
	// structural walk so the inventory is complete and the
	// Fatal-is-last contract holds (the structural/accounting passes
	// have already returned on any fatal; checkIndexes emits only
	// warnings + FingerprintDrift errors, never fatals).
	if c.checkIndexesEnabled() && !c.Stopped {
		c.checkIndexes(hwm)
	}
}

// walkKeyspaces enumerates keyspaces from the snapshot's KeyspaceRoot,
// validates each descriptor, and walks each keyspace's data tree +
// index registry + index data trees into the reachable set. Enumeration
// uses the hwm-guarded btree.WalkKV (NOT the read cursor, whose descent
// is unguarded and would panic/SIGBUS on a corrupt or forged tree —
// Check must not crash on the corruption it exists to detect).
func (c *Checker) walkKeyspaces(firstData, hwm uint64) bool {
	// The keyspace-descriptor tree itself is order-validated too — a
	// routing flip there makes OpenKeyspace descent miss a keyspace
	// mid-op while every per-page check stays clean.
	if _, _, ok, _ := c.validateTreeOrder("", "(keyspace tree)", c.Meta.KeyspaceRoot, 0, hwm); !ok {
		return false
	}
	var keyspaceCount uint64
	err := btree.WalkKV(RawPageReader{c.Pgr}, c.Cfg, c.Meta.KeyspaceRoot, hwm, func(k, v []byte) error {
		name := string(k)
		keyspaceCount++
		if len(v) != descriptor.Size {
			if !c.Emit(Issue{Severity: Error, Code: CodeKeyspaceDescriptorSize, Keyspace: name,
				Message: fmt.Sprintf("descriptor value length %d != %d", len(v), descriptor.Size)}) {
				return errCheckStop
			}
			return nil
		}
		desc := descriptor.Decode(v)
		if verr := descriptor.Validate(v, desc); verr != nil {
			if !c.Emit(Issue{Severity: Error, Code: CodeKeyspaceDescriptorInvalid, Keyspace: name,
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
			if desc.Kind == descriptor.KindSetKeyspace {
				want, unit = values, "values"
			}
			if desc.Count != want {
				if !c.Emit(Issue{Severity: Error, Code: CodeKeyspaceCountMismatch, Keyspace: name,
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
		if desc.Kind == descriptor.KindSetKeyspace {
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
	if err == nil && keyspaceCount != c.Meta.NumKeyspaces {
		if !c.Emit(Issue{Severity: Error, Code: CodeNumKeyspacesMismatch,
			Message: fmt.Sprintf("meta.NumKeyspaces %d, descriptor tree holds %d", c.Meta.NumKeyspaces, keyspaceCount)}) {
			return false
		}
	}
	return c.dispositionEnumErr(err, "KeyspaceWalkFailed", "", "keyspace enumeration")
}

// validateTreeOrder runs the tree-level ordering/consistency pass
// (api-surface.md §Check: key ordering, separator routing,
// nested-tree member counts, descriptor counts) that per-page
// checksums and structural Validate cannot see. One extra read pass
// over the keyspace's live pages; violations are Error. A
// structural walk failure is NOT re-reported here — walkTree already
// ran on this root.
func (c *Checker) validateTreeOrder(ks, idx string, root uint64, fvs uint16, hwm uint64) (entries, values uint64, ok, structural bool) {
	stopped := false
	entries, values, err := btree.ValidateOrder(RawPageReader{c.Pgr}, c.Cfg, root, hwm, fvs,
		func(kind btree.OrderViolationKind, pageID uint64, msg string) bool {
			code := "KeyOrderViolation"
			if kind == btree.OrderNestedCount {
				code = "NestedCountMismatch"
			}
			if !c.Emit(Issue{Severity: Error, Code: code, PageID: pageID, Keyspace: ks, Index: idx, Message: msg}) {
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
// a SubpageCorrupt Error. A WalkLeafEntries structural failure is NOT
// re-reported here — the reachability walkTree pass already ran and
// reported the tree-structure corruption. Returns false only when the
// caller stopped iterating.
func (c *Checker) checkSetKeyspaceSubpages(ks string, dataRoot uint64, fvs uint16, hwm uint64) bool {
	if dataRoot == 0 {
		return true
	}
	err := btree.WalkLeafEntries(RawPageReader{c.Pgr}, c.Cfg, dataRoot, hwm, func(e page.LeafEntry) error {
		if !e.IsSubpage() {
			return nil // nested-tree / other cells are covered by the reachability walk
		}
		// NewSubpageReader panics below SubpageHeaderSize and Validate is
		// not total over a malformed header — bound the length first, the
		// same guard the CheckIndexes path uses.
		if len(e.Value) < page.SubpageHeaderSize {
			if !c.Emit(Issue{Severity: Error, Code: CodeSubpageCorrupt, Keyspace: ks,
				Message: fmt.Sprintf("set key %q subpage is %d bytes (< header %d)", e.Key, len(e.Value), page.SubpageHeaderSize)}) {
				return errCheckStop
			}
			return nil
		}
		if verr := page.NewSubpageReader(e.Value, fvs).Validate(); verr != nil {
			if !c.Emit(Issue{Severity: Error, Code: CodeSubpageCorrupt, Keyspace: ks,
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
func (c *Checker) walkRegistry(ks string, desc descriptor.Keyspace, firstData, hwm uint64) bool {
	if !c.walkTree(ks, "", desc.IndexRegistryRoot, firstData, hwm) {
		return false
	}
	if _, _, ok, _ := c.validateTreeOrder(ks, "(index registry)", desc.IndexRegistryRoot, 0, hwm); !ok {
		return false
	}
	err := btree.WalkKV(RawPageReader{c.Pgr}, c.Cfg, desc.IndexRegistryRoot, hwm, func(k, v []byte) error {
		idxName := string(k)
		entry, derr := indexing.DecodeRegistryEntry(v)
		if derr != nil {
			if !c.Emit(Issue{Severity: Error, Code: CodeRegistryEntryInvalid, Keyspace: ks, Index: idxName,
				Message: fmt.Sprintf("registry entry decode: %v", derr)}) {
				return errCheckStop
			}
			return nil
		}
		// Kind + padding assertions (indexing.md §Storage Layout):
		// the decoder tolerates nonzero padding and unknown kinds
		// structurally; the strict walk is where both surface. An
		// unknown kind is exactly what open rejects — the walk must
		// not silently pass what open refuses.
		foreignKind := entry.Kind != indexing.KindComposite
		if foreignKind {
			if !c.Emit(Issue{Severity: Error, Code: CodeRegistryEntryKindUnknown, Keyspace: ks, Index: idxName,
				Message: fmt.Sprintf("registry entry kind %d unknown to this engine version", entry.Kind)}) {
				return errCheckStop
			}
			// Root is kind-agnostic spec contract (indexing.md
			// §Storage Layout), so the STRUCTURAL walk below still
			// runs — checksums verify and the pages count as
			// reachable (no spurious BitmapLeak). Only the
			// composite-semantic order pass is skipped.
		}
		for i := 10; i < 16; i++ {
			if v[i] != 0 {
				if !c.Emit(Issue{Severity: Error, Code: CodeRegistryEntryPaddingNonzero, Keyspace: ks, Index: idxName,
					Message: fmt.Sprintf("registry entry padding byte %d is 0x%02x, want 0", i-10, v[i])}) {
					return errCheckStop
				}
				break
			}
		}
		if c.checkIndexesEnabled() {
			if info := c.inv[ks]; info != nil {
				info.indexRoots[idxName] = entry.Root
			}
		}
		if !c.walkTree(ks, idxName, entry.Root, firstData, hwm) {
			return errCheckStop
		}
		if !foreignKind {
			if _, _, ok, _ := c.validateTreeOrder(ks, idxName, entry.Root, 0, hwm); !ok {
				return errCheckStop
			}
		}
		return nil
	})
	return c.dispositionEnumErr(err, "RegistryWalkFailed", ks, "index registry enumeration")
}

// dispositionEnumErr maps a WalkKV result: nil → true (continue);
// errCheckStop → false (caller stopped); a structural failure → a
// Fatal issue, after which the whole walk halts so the fatal is the
// LAST issue yielded (api-surface.md §Check). It latches stopped and
// returns false so run() does not proceed to walkRPL / accounting (whose
// findings would otherwise follow the fatal, and would be spurious since
// the aborted enumeration left the reachable set incomplete).
func (c *Checker) dispositionEnumErr(err error, code, ks, what string) bool {
	if err == nil {
		return true
	}
	if errors.Is(err, errCheckStop) {
		return false
	}
	c.Emit(Issue{Severity: Fatal, Code: code, Keyspace: ks,
		Message: fmt.Sprintf("%s failed: %v", what, err)})
	c.Stopped = true
	return false
}

// walkTree walks one B+tree rooted at root, verifying each page's
// checksum + structure and recording it in the reachable set. A walk
// failure (corrupt/forged page) is reported as an Error for ks/idx
// and does not abort the overall Check (the next tree still runs).
// Returns false only when the caller stopped iterating.
func (c *Checker) walkTree(ks, idx string, root, firstData, hwm uint64) bool {
	if root == 0 {
		return true
	}
	visit := func(id uint64, kind btree.PageKind, depth int) error {
		if id < firstData {
			if !c.Emit(Issue{Severity: Error, Code: CodePointerIntoReservedRegion, PageID: id, Keyspace: ks, Index: idx,
				Message: fmt.Sprintf("tree page %d lies in the reserved meta/bitmap region (< %d)", id, firstData)}) {
				return errCheckStop
			}
			return nil
		}
		if c.reachable.Test(id) {
			if !c.Emit(Issue{Severity: Error, Code: CodePageDoubleReferenced, PageID: id, Keyspace: ks, Index: idx,
				Message: fmt.Sprintf("page %d is reachable from more than one parent", id)}) {
				return errCheckStop
			}
			return nil
		}
		c.reachable.Set(id)
		if c.Cfg.PageChecksum {
			if !page.VerifyPageFooter(c.Pgr.PageRaw(id), c.Cfg.PageSize) {
				if !c.Emit(Issue{Severity: Error, Code: CodeBadPageChecksum, PageID: id, Keyspace: ks, Index: idx,
					Message: fmt.Sprintf("page %d checksum mismatch", id)}) {
					return errCheckStop
				}
			}
		}
		return nil
	}
	err := btree.Walk(RawPageReader{c.Pgr}, c.Cfg, root, hwm, visit)
	if err == nil {
		return true
	}
	if errors.Is(err, errCheckStop) {
		return false
	}
	// A structural walk failure: report and continue with other trees.
	sev := Error
	code := "TreeCorrupted"
	if errors.Is(err, btree.ErrTreeTooDeep) {
		code = "TreeCycleOrTooDeep"
	}
	return c.Emit(Issue{Severity: sev, Code: code, Keyspace: ks, Index: idx,
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
// Hard walk errors surface as per-kind Error issues. firstData is
// run()'s first-data-page boundary (the meta/bitmap region ends there).
// Returns ok=false only when the caller stopped iterating.
func (c *Checker) walkRPL(firstData, hwm uint64, bm *bitmap.Bitmap, bmOK bool) (Bitset, bool) {
	pending := NewBitset(hwm)
	head := c.Meta.RPLHeadPage
	if head == 0 {
		return pending, true
	}
	// bm is run()'s single snapshot of the allocation bitmap — the
	// reclaimed-segment oracle (shared with accounting so the two
	// passes cannot disagree); if unavailable the walk falls back to
	// the footer/decode boundary alone.
	walk := pager.RPLChainWalk{
		ReadPage:     c.Pgr.PageRaw,
		Cfg:          c.Cfg,
		Head:         head,
		HeadTxnID:    c.Meta.RPLHeadTxnID,
		Tail:         c.Meta.RPLTailPage,
		EntryCount:   c.Meta.RPLEntryCount,
		ReclaimEpoch: c.Meta.Durable.TxnID,
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
		pending.SetIfInRange(id)
		for _, pid := range seg.PageIDs {
			pending.SetIfInRange(pid)
		}
		return true
	})
	if werr != nil {
		return pending, c.Emit(rplWalkIssue(werr, hwm))
	}
	// A footer/decode boundary is an AMBIGUOUS truncation: unlike the
	// reclaimed boundary (whose behind-boundary segments every walker
	// whose chain derives from the current image agrees are gone), a
	// corrupt segment may have bitrotted AFTER the live writer built
	// its in-memory chain, so segments behind it may still be live
	// pending state. Latch the ambiguity for the reclamation gates
	// (maintReclaimLeaks / Repair) and surface BOTH boundary kinds —
	// pre-fix the decode boundary was silent, hiding the truncation
	// from the operator.
	switch stop.Reason {
	case pager.RPLWalkFooterBoundary:
		c.RPLBoundary = true
		if !c.Emit(Issue{Severity: Warning, Code: CodeRPLSegmentChecksum, PageID: stop.PageID,
			Message: fmt.Sprintf("RPL segment page %d fails checksum; chain walk stopped before tail %d (pages behind the boundary surface as BitmapLeak until reclamation quarantines the segment)", stop.PageID, c.Meta.RPLTailPage)}) {
			return pending, false
		}
	case pager.RPLWalkDecodeBoundary:
		c.RPLBoundary = true
		if !c.Emit(Issue{Severity: Warning, Code: CodeRPLSegmentBoundary, PageID: stop.PageID,
			Message: fmt.Sprintf("RPL segment page %d does not decode; chain walk stopped before tail %d (pages behind the boundary surface as BitmapLeak until reclamation quarantines the segment)", stop.PageID, c.Meta.RPLTailPage)}) {
			return pending, false
		}
	}
	return pending, true
}

// rplWalkIssue maps a hard chain-walk failure to Check's stable issue
// codes. hwm is walkRPL's walk bound (the clamped HighWaterMark), named
// in the out-of-range message.
func rplWalkIssue(werr *pager.RPLWalkError, hwm uint64) Issue {
	iss := Issue{Severity: Error, PageID: werr.PageID}
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
func (c *Checker) accounting(firstData, hwm uint64, rplPages Bitset, bm *bitmap.Bitmap, bmOK bool) {
	if !bmOK {
		c.Emit(Issue{Severity: Warning, Code: CodeBitmapUnavailable,
			Message: "could not read allocation bitmap from snapshot; page accounting skipped"})
		return
	}
	for id := firstData; id < hwm; id++ {
		free := bm.IsSet(id) // true = free
		reach := c.reachable.Test(id)
		pending := rplPages.Test(id)
		// Partition (api-surface.md §Check, CopyTo, Compact): a data page is exactly one of {reachable,
		// free, RPL-pending}. Any overlap is corruption.
		switch {
		case reach && free:
			if !c.Emit(Issue{Severity: Error, Code: CodeReachableButFree, PageID: id,
				Message: fmt.Sprintf("page %d is referenced by the tree but marked free in the bitmap", id)}) {
				return
			}
		case reach && pending:
			if !c.Emit(Issue{Severity: Error, Code: CodeReachableInRPL, PageID: id,
				Message: fmt.Sprintf("page %d is referenced by the tree but also pending RPL reclamation", id)}) {
				return
			}
		case !reach && free && pending:
			// A page on the free list AND in the RPL will be set free a
			// second time when reclamation processes its segment — a
			// future double-allocation hazard.
			if !c.Emit(Issue{Severity: Error, Code: CodeFreeAndPending, PageID: id,
				Message: fmt.Sprintf("page %d is both free in the bitmap and pending RPL reclamation", id)}) {
				return
			}
		case !reach && !free && !pending:
			if c.Repair {
				// Defer emission: checkRepair frees these after the walk
				// (Repair needs the COMPLETE reachable set first) and emits
				// each with Repaired set per the outcome.
				c.Leaked = append(c.Leaked, id)
			} else if !c.EmitLeak(id, false) {
				return
			}
		}
	}
	// NumFree consistency (advisory under concurrent writes).
	if got := bm.Recount(); got != c.Meta.NumFreePages {
		c.Emit(Issue{Severity: Warning, Code: CodeFreeCountMismatch,
			Message: fmt.Sprintf("bitmap free-page count %d != meta NumFreePages %d", got, c.Meta.NumFreePages)})
	}
}

// checkIndexes runs the extractor-equivalence pass (Options.
// CheckIndexes). For each indexed keyspace not covered by opts.Indexes it
// emits KeyspaceNotSupplied; for each supplied (keyspace, indexing.Decl) it
// flags KeyspaceNotFound / IndexNotInRegistry on misconfiguration, else
// verifies the stored index equals what the extractor re-run over every
// row produces. Read-only; emits only warnings + FingerprintDrift errors,
// never Fatal, so the Fatal-is-last contract is preserved. Map
// iteration order is unspecified, but findings are a set so order is
// immaterial.
func (c *Checker) checkIndexes(hwm uint64) {
	// Indexed keyspaces the caller supplied no extractors for.
	for ksName, info := range c.inv {
		if len(info.indexRoots) == 0 {
			continue // not an indexed keyspace
		}
		if _, ok := c.Opts.Indexes[ksName]; ok {
			continue
		}
		if !c.Emit(Issue{Severity: Warning, Code: CodeCheckIndexesKeyspaceNotSupplied, Keyspace: ksName,
			Message: fmt.Sprintf("indexed keyspace %q has no IndexDecls supplied; extractor-equivalence skipped (structure still checked)", ksName)}) {
			return
		}
	}
	// Supplied extractors → verify, or flag misconfiguration.
	for ksName, decls := range c.Opts.Indexes {
		info, ok := c.inv[ksName]
		if !ok {
			if !c.Emit(Issue{Severity: Warning, Code: CodeCheckIndexesKeyspaceNotFound, Keyspace: ksName,
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
				if !c.Emit(Issue{Severity: Warning, Code: CodeCheckIndexesIndexNotInRegistry, Keyspace: ksName, Index: decl.Name,
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
// index data tree. A discrepancy is a FingerprintDrift Error (the
// index does not match the supplied extractor — typically an extractor
// changed without a Version bump, indexing.md §Drift Guard). Both walks
// use the guarded WalkKV (no panic on a forged tree). Returns false only
// when the caller stopped iterating.
func (c *Checker) verifyIndexEquivalence(ks string, info *ksInventory, decl *indexing.Decl, idxRoot, hwm uint64) bool {
	if decl.Extract == nil {
		return c.Emit(Issue{Severity: Error, Code: CodeCheckIndexesExtractorMissing, Keyspace: ks, Index: decl.Name,
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
	case descriptor.KindKeyspace:
		expected, extractErr, structErr = c.expectedKeyspaceIndex(decl, info.dataRoot, hwm, hasCovering)
	case descriptor.KindSetKeyspace:
		expected, extractErr, structErr = c.expectedSetKeyspaceIndex(decl, info.dataRoot, info.fixedValueSize, hwm, hasCovering)
	default:
		return c.Emit(Issue{Severity: Warning, Code: CodeCheckIndexesKeyspaceKindUnsupported, Keyspace: ks, Index: decl.Name,
			Message: fmt.Sprintf("keyspace %q has kind %d, which CheckIndexes cannot verify", ks, info.kind)})
	}
	if extractErr != nil {
		return c.Emit(Issue{Severity: Error, Code: CodeCheckIndexesExtractorError, Keyspace: ks, Index: decl.Name,
			Message: fmt.Sprintf("re-running extractor failed: %v", extractErr)})
	}
	if structErr != nil {
		// Structural failure enumerating rows/members (already reported by
		// the structural pass); skip equivalence for this index.
		return c.Emit(Issue{Severity: Warning, Code: CodeCheckIndexesRowsUnreadable, Keyspace: ks, Index: decl.Name,
			Message: fmt.Sprintf("could not enumerate rows/members for index verification: %v", structErr)})
	}
	// Stored entries: enumerate the index data tree.
	stored := make(map[string]string)
	serr := btree.WalkKV(RawPageReader{c.Pgr}, c.Cfg, idxRoot, hwm, func(k, v []byte) error {
		stored[string(k)] = string(v)
		return nil
	})
	if serr != nil {
		return c.Emit(Issue{Severity: Warning, Code: CodeCheckIndexesIndexUnreadable, Keyspace: ks, Index: decl.Name,
			Message: fmt.Sprintf("could not enumerate stored index entries: %v", serr)})
	}
	if missing, extra, mism := diffEntrySets(expected, stored); missing > 0 || extra > 0 || mism > 0 {
		return c.Emit(Issue{Severity: Error, Code: CodeCheckIndexesFingerprintDrift, Keyspace: ks, Index: decl.Name,
			Message: fmt.Sprintf("index %q drift: %d expected entries missing from the index, %d stored entries the extractor did not produce, %d value mismatches",
				decl.Name, missing, extra, mism)})
	}
	return true
}

// expectedKeyspaceIndex re-runs decl.Extract over every row of a plain
// Keyspace's data tree and returns the (encoded key → encoded value) set
// the index should hold, using the Keyspace codec (row key as PK).
func (c *Checker) expectedKeyspaceIndex(decl *indexing.Decl, dataRoot, hwm uint64, hasCovering bool) (expected map[string]string, extractErr, structErr error) {
	expected = make(map[string]string)
	werr := btree.WalkKV(RawPageReader{c.Pgr}, c.Cfg, dataRoot, hwm, func(k, v []byte) error {
		entries, eerr := c.ExtractKeySet(decl, k, v)
		if eerr != nil {
			extractErr = eerr
			return errCheckStop
		}
		for ek, entry := range entries {
			expected[ek] = string(indexing.EntryValue(entry, k, decl.Unique, hasCovering))
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
// encoded value) set, using the SetKeyspace codec (indexing.EncodeSetEntryKey
// + the compound (setKey,member) PK). Members live in a subpage (small
// sets) or a nested B+tree keyed by the set key in the outer tree; both
// are enumerated through the guarded walkers so a forged tree yields an
// error, not a panic.
func (c *Checker) expectedSetKeyspaceIndex(decl *indexing.Decl, dataRoot uint64, fvs uint16, hwm uint64, hasCovering bool) (expected map[string]string, extractErr, structErr error) {
	expected = make(map[string]string)
	pr := RawPageReader{c.Pgr}
	addMember := func(setKey, member []byte) error {
		entries, eerr := c.SetExtractKeySet(decl, setKey, member)
		if eerr != nil {
			extractErr = eerr
			return errCheckStop
		}
		compoundPK := indexing.EncodeSetCompoundPK(setKey, member)
		for ek, entry := range entries {
			expected[ek] = string(indexing.EntryValue(entry, compoundPK, decl.Unique, hasCovering))
		}
		return nil
	}
	werr := btree.WalkLeafEntries(pr, c.Cfg, dataRoot, hwm, func(e page.LeafEntry) error {
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
			return btree.WalkKV(pr, c.Cfg, e.NestedRoot, hwm, func(member, _ []byte) error {
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

// SnapshotBitmap reconstructs the allocation bitmap from the snapshot's
// on-disk bitmap region (pages [2, 2+BitmapPages)). The detail bytes are
// copied out of the mmap so the returned bitmap is independent of any
// concurrent in-place bitmap pwrite.
func (c *Checker) SnapshotBitmap() (*bitmap.Bitmap, bool) {
	if c.Meta.BitmapPages == 0 {
		return nil, false
	}
	ps := uint64(c.Cfg.PageSize)
	detail := make([]byte, uint64(c.Meta.BitmapPages)*ps)
	for i := uint64(0); i < uint64(c.Meta.BitmapPages); i++ {
		copy(detail[i*ps:(i+1)*ps], c.Pgr.PageRaw(2+i))
	}
	return bitmap.New(detail, c.Cfg.PageSize, c.Meta.BitmapPages, c.Meta.MaxSize), true
}

// Bitset is a compact page-id membership set sized to a known upper
// bound (the snapshot HighWaterMark) — 1 bit per page, so 8 MB covers a
// 256 GB / 4 KB database, vs a map's per-entry overhead.
type Bitset struct {
	words []uint64
	n     uint64
}

func NewBitset(n uint64) Bitset {
	return Bitset{words: make([]uint64, (n+63)/64), n: n}
}

func (b Bitset) Set(id uint64) {
	if id < b.n {
		b.words[id>>6] |= 1 << (id & 63)
	}
}

// SetIfInRange is Set but silently ignores ids outside [0, n) — used
// for RPL-listed page ids which a forged segment could push out of
// range (already reported elsewhere; here we just avoid OOB).
func (b Bitset) SetIfInRange(id uint64) { b.Set(id) }

func (b Bitset) Test(id uint64) bool {
	if id >= b.n {
		return false
	}
	return b.words[id>>6]&(1<<(id&63)) != 0
}

// TreePageStats tallies B+tree page kinds and the maximum descent depth.
type TreePageStats struct {
	Depth         int // number of levels (root→leaf); 0 for an empty tree
	BranchPages   uint64
	LeafPages     uint64
	OverflowPages uint64
}

// WalkTreePageStats walks the B+tree rooted at root (a no-op for root ==
// 0) and tallies branch / leaf / overflow page counts and the max
// branch-or-leaf descent depth. Overflow pages are counted but do NOT
// contribute to depth (Walk reports them at their leaf's depth + 1). For
// a SetKeyspace the walk recurses into nested set-member subtrees, so
// the depth is the deepest path including nesting. O(tree pages).
func WalkTreePageStats(pr btree.PageReader, cfg page.Config, root, hwm uint64) (TreePageStats, error) {
	var s TreePageStats
	maxLevel := -1
	err := btree.Walk(pr, cfg, root, hwm, func(_ uint64, kind btree.PageKind, depth int) error {
		switch kind {
		case btree.PageKindBranch:
			s.BranchPages++
			if depth > maxLevel {
				maxLevel = depth
			}
		case btree.PageKindLeaf:
			s.LeafPages++
			if depth > maxLevel {
				maxLevel = depth
			}
		case btree.PageKindOverflow:
			s.OverflowPages++
		}
		return nil
	})
	if err != nil {
		return TreePageStats{}, err
	}
	s.Depth = maxLevel + 1 // empty tree: maxLevel stays -1 ⇒ depth 0
	return s, nil
}

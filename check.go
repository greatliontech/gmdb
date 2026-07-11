package gmdb

import (
	"context"
	"fmt"
	"iter"

	"github.com/thegrumpylion/gmdb/internal/lock"
	"github.com/thegrumpylion/gmdb/internal/page"
	"github.com/thegrumpylion/gmdb/internal/pager"
	"github.com/thegrumpylion/gmdb/internal/verify"
)

// CheckSeverity classifies a CheckIssue: CheckWarning (non-critical
// finding), CheckError (structural integrity violation), CheckFatal
// (the walk could not continue; always the last issue yielded).
//
// The concrete types live in internal/verify; these aliases are the
// public surface.
type CheckSeverity = verify.Severity

const (
	CheckWarning = verify.Warning
	CheckError   = verify.Error
	CheckFatal   = verify.Fatal
)

// CheckIssue is one finding from a Check walk. See api-surface.md
// §Check, CopyTo, Compact. Code is a stable machine-parseable token;
// Message is free-form human-facing text (do not pattern-match on it).
type CheckIssue = verify.Issue

// CheckOptions configures CheckWithOptions. A nil *CheckOptions (or the
// zero value) is plain structural Check. See internal/verify for the
// field-level contracts (Repair exclusivity + conservatism,
// CheckIndexes extractor re-run semantics, the Indexes decl map).
type CheckOptions = verify.Options

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
		c := &verify.Checker{
			Pgr:   rtx.pgr,
			Cfg:   page.Config{PageSize: meta.PageSize, PageChecksum: meta.HasFlag(pager.MetaFlagPageChecksum)},
			Meta:  meta,
			Yield: yield,
			Opts:  opts,

			ExtractKeySet:    extractEntriesAsKeySet,
			SetExtractKeySet: setKeyspaceExtractEntries,
		}
		c.Run()
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
		c := &verify.Checker{
			Pgr:    tx.pgr,
			Cfg:    page.Config{PageSize: meta.PageSize, PageChecksum: meta.HasFlag(pager.MetaFlagPageChecksum)},
			Meta:   meta,
			Yield:  yield,
			Opts:   opts,
			Repair: true,

			ExtractKeySet:    extractEntriesAsKeySet,
			SetExtractKeySet: setKeyspaceExtractEntries,
		}
		c.Run()

		// Completeness gate (api-surface.md §Check, CopyTo, Compact): free only when the walk ran to
		// completion (caller did not break) and reported no structural
		// error/fatal. Otherwise the reachable set is unreliable and a
		// live page could be misclassified as leaked.
		if c.Stopped {
			return // caller broke, or a CheckFatal already terminated the walk
		}
		if c.SawError {
			// Corruption present: report the leaks we found, unrepaired,
			// then a Skipped warning. Reclaim nothing.
			for _, id := range c.Leaked {
				if !c.EmitLeak(id, false) {
					return
				}
			}
			c.Emit(CheckIssue{Severity: CheckWarning, Code: "Repair.Skipped",
				Message: "structural corruption present; leaked pages reported but not reclaimed (reachable set unreliable)"})
			return
		}
		if c.RPLBoundary {
			// The RPL walk truncated at a corrupt-segment boundary: the
			// leaked set may intersect the live writer's in-memory chain
			// (this process frees those entries again when its
			// reclamation reaches the segment — a double-free under any
			// page re-allocated in between). Report unrepaired + skip,
			// the structural-findings shape.
			for _, id := range c.Leaked {
				if !c.EmitLeak(id, false) {
					return
				}
			}
			c.Emit(CheckIssue{Severity: CheckWarning, Code: "Repair.Skipped",
				Message: "RPL chain walk stopped at a corrupt-segment boundary; leaked pages reported but not reclaimed (the set may intersect the live RPL)"})
			return
		}
		if len(c.Leaked) == 0 {
			return // structurally clean, no leaks — nothing to commit
		}

		// Free exactly the leaked set in the bitmap and publish via commit.
		for _, id := range c.Leaked {
			if err := tx.pgr.FreeLeakedPage(id); err != nil {
				c.Emit(CheckIssue{Severity: CheckFatal, Code: "Repair.FreeFailed", PageID: id,
					Message: fmt.Sprintf("could not free leaked page %d: %v", id, err)})
				return
			}
		}
		if err := tx.Commit(); err != nil {
			c.Emit(CheckIssue{Severity: CheckFatal, Code: "Repair.CommitFailed",
				Message: fmt.Sprintf("repair commit failed; no pages reclaimed: %v", err)})
			return
		}
		committed = true
		for _, id := range c.Leaked {
			if !c.EmitLeak(id, true) {
				return
			}
		}
	}
}

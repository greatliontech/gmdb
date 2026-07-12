package gmdb

import (
	"testing"

	"github.com/thegrumpylion/gmdb/internal/verify"
)

// CheckIssue.Code tokens are a documented stable contract
// (api-surface.md §CheckIssue: "existing ones never change
// meaning") — external tooling pattern-matches them. Every token
// is single-sourced as a constant and its VALUE pinned here, so a
// mechanical rename can never change a documented token with a
// green suite (the incident on record: "keyspaceDescriptorSize"
// — its historical lower-case k included — was once renamed
// silently). New codes extend this table in the same change.
func TestCheckIssueCodeTokensPinned(t *testing.T) {
	for _, tc := range []struct{ got, want string }{
		{verify.CodeBadPageChecksum, "BadPageChecksum"},
		{verify.CodeBitmapLeak, "BitmapLeak"},
		{verify.CodeBitmapUnavailable, "BitmapUnavailable"},
		{verify.CodeCheckIndexesExtractorError, "CheckIndexes.ExtractorError"},
		{verify.CodeCheckIndexesExtractorMissing, "CheckIndexes.ExtractorMissing"},
		{verify.CodeCheckIndexesFingerprintDrift, "CheckIndexes.FingerprintDrift"},
		{verify.CodeCheckIndexesIndexNotInRegistry, "CheckIndexes.IndexNotInRegistry"},
		{verify.CodeCheckIndexesIndexUnreadable, "CheckIndexes.IndexUnreadable"},
		{verify.CodeCheckIndexesKeyspaceKindUnsupported, "CheckIndexes.KeyspaceKindUnsupported"},
		{verify.CodeCheckIndexesKeyspaceNotFound, "CheckIndexes.KeyspaceNotFound"},
		{verify.CodeCheckIndexesKeyspaceNotSupplied, "CheckIndexes.KeyspaceNotSupplied"},
		{verify.CodeCheckIndexesRowsUnreadable, "CheckIndexes.RowsUnreadable"},
		{verify.CodeFreeAndPending, "FreeAndPending"},
		{verify.CodeFreeCountMismatch, "FreeCountMismatch"},
		{verify.CodeHighWaterMarkOutOfRange, "HighWaterMarkOutOfRange"},
		{verify.CodeKeyspaceCountMismatch, "KeyspaceCountMismatch"},
		{verify.CodeKeyspaceDescriptorInvalid, "KeyspaceDescriptorInvalid"},
		{verify.CodeMetaInvalid, "MetaInvalid"},
		{verify.CodeNumKeyspacesMismatch, "NumKeyspacesMismatch"},
		{verify.CodePageDoubleReferenced, "PageDoubleReferenced"},
		{verify.CodePointerIntoReservedRegion, "PointerIntoReservedRegion"},
		{verify.CodeRPLSegmentBoundary, "RPLSegmentBoundary"},
		{verify.CodeRPLSegmentChecksum, "RPLSegmentChecksum"},
		{verify.CodeReachableButFree, "ReachableButFree"},
		{verify.CodeReachableInRPL, "ReachableInRPL"},
		{verify.CodeRegistryEntryInvalid, "RegistryEntryInvalid"},
		{verify.CodeRegistryEntryKindUnknown, "RegistryEntryKindUnknown"},
		{verify.CodeRegistryEntryPaddingNonzero, "RegistryEntryPaddingNonzero"},
		{verify.CodeSubpageCorrupt, "SubpageCorrupt"},
		{verify.CodeKeyspaceDescriptorSize, "keyspaceDescriptorSize"},
		{codeReadTxUnavailable, "ReadTxUnavailable"},
		{codeRepairCommitFailed, "Repair.CommitFailed"},
		{codeRepairFreeFailed, "Repair.FreeFailed"},
		{codeRepairReadersActive, "Repair.ReadersActive"},
		{codeRepairSkipped, "Repair.Skipped"},
		{codeRepairWriteTxUnavailable, "Repair.WriteTxUnavailable"},
	} {
		if tc.got != tc.want {
			t.Errorf("issue code %q drifted from the documented token %q", tc.got, tc.want)
		}
	}
}

package verify

// Issue codes: the stable, machine-parseable tokens the check
// walk emits (api-surface.md §CheckIssue — "existing ones never
// change meaning"). One constant per token is the single source;
// the root package pins every value so a rename cannot change a
// documented token silently (that exact incident is on record:
// "keyspaceDescriptorSize" — note its historical casing — was
// once mechanically renamed with a green suite).
const (
	CodeBadPageChecksum                     = "BadPageChecksum"
	CodeBitmapLeak                          = "BitmapLeak"
	CodeBitmapUnavailable                   = "BitmapUnavailable"
	CodeCheckIndexesExtractorError          = "CheckIndexes.ExtractorError"
	CodeCheckIndexesExtractorMissing        = "CheckIndexes.ExtractorMissing"
	CodeCheckIndexesFingerprintDrift        = "CheckIndexes.FingerprintDrift"
	CodeCheckIndexesIndexNotInRegistry      = "CheckIndexes.IndexNotInRegistry"
	CodeCheckIndexesIndexUnreadable         = "CheckIndexes.IndexUnreadable"
	CodeCheckIndexesKeyspaceKindUnsupported = "CheckIndexes.KeyspaceKindUnsupported"
	CodeCheckIndexesKeyspaceNotFound        = "CheckIndexes.KeyspaceNotFound"
	CodeCheckIndexesKeyspaceNotSupplied     = "CheckIndexes.KeyspaceNotSupplied"
	CodeCheckIndexesRowsUnreadable          = "CheckIndexes.RowsUnreadable"
	CodeFreeAndPending                      = "FreeAndPending"
	CodeFreeCountMismatch                   = "FreeCountMismatch"
	CodeHighWaterMarkOutOfRange             = "HighWaterMarkOutOfRange"
	CodeKeyspaceCountMismatch               = "KeyspaceCountMismatch"
	CodeKeyspaceDescriptorInvalid           = "KeyspaceDescriptorInvalid"
	CodeMetaInvalid                         = "MetaInvalid"
	CodeNumKeyspacesMismatch                = "NumKeyspacesMismatch"
	CodePageDoubleReferenced                = "PageDoubleReferenced"
	CodePointerIntoReservedRegion           = "PointerIntoReservedRegion"
	CodeRPLSegmentBoundary                  = "RPLSegmentBoundary"
	CodeRPLSegmentChecksum                  = "RPLSegmentChecksum"
	CodeReachableButFree                    = "ReachableButFree"
	CodeReachableInRPL                      = "ReachableInRPL"
	CodeRegistryEntryInvalid                = "RegistryEntryInvalid"
	CodeRegistryEntryKindUnknown            = "RegistryEntryKindUnknown"
	CodeRegistryEntryPaddingNonzero         = "RegistryEntryPaddingNonzero"
	CodeSubpageCorrupt                      = "SubpageCorrupt"
	CodeKeyspaceDescriptorSize              = "keyspaceDescriptorSize"
)

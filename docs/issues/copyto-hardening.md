# CopyTo: SIGBUS on truncated source, torn destination on crash; Check overflow gap

Lands: 19

## Findings

**[M] `CopyTo(compact=false)` SIGBUS on a truncated or forged-meta
source file.** `copy.go:80` (`collectReachable` via `rawPageReader`),
`copy.go:123` (`PageRaw` copy loop): the verbatim path walks and copies
to the snapshot meta's `HighWaterMark` unclamped, through `PageRaw`,
which is explicitly unbounded against the file-resident extent
(`internal/pager/pager.go:830-861`). `Check` clamps hwm to
`fileSize/PageSize` for exactly this reason (`check.go:354-366`);
CopyTo does not. A file truncated by an incomplete transfer (meta
checksum still valid) → SIGBUS, process death — violating
checksums.md's error-not-crash contract for corrupt surfaces
(`copyCompact` is safe: the verifying `Page` accessor bounds ids).

**[M] CopyTo destination is not crash-consistent: a torn backup is
indistinguishable from a complete one.** `copy.go:88-186`: data →
bitmap → both metas written directly at the final path, single fsync at
the end. Power loss mid-copy can persist valid metas without data; the
destination then opens successfully and either fails reads with
ErrBadPageChecksum or, with DisablePageChecksum, silently serves
garbage. A retry hits O_EXCL failure, reinforcing "backup already
done". `Compact()` already uses temp-file + atomic rename
(`compact.go:89`); CopyTo does not. Spec-amend rider: api-surface.md
pins "path must not exist" and fresh-UUID but is silent on destination
crash-consistency — pin temp+rename (surfaced in the audit spec-amend
list, together with scoping checksums.md's error-not-crash contract to
cover CopyTo/Compact walks).

**[L] Check false negative: overflow-run headers never structurally
validated.** `check.go:630-675` + `internal/btree/walk.go:218-230`:
overflow page ids are enumerated from the leaf's TotalLen and only
footer-verified; the first page's TypeOverflow header/AdditionalPages
is never cross-checked. With DisablePageChecksum (or a forged footer),
a corrupted overflow header passes `Check()` clean while every `Get`
of that key fails ErrCorrupted.

## Fix direction

Clamp the verbatim walk/copy to the file-resident extent (Check's
clamp); switch CopyTo to temp+rename with the spec amendment; validate
overflow-run headers in the Check walk. Regression: truncated-source
CopyTo returns an error (no crash); overflow-header corruption is
reported by Check.

## Provenance

2026-07-10 defect audit; bulkload/copy/check reviewer.

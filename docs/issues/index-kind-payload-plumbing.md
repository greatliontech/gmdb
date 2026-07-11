# Pinned index state does not carry the registry kind payload

Lands: when the first non-composite index kind is implemented

## Finding

The registry entry v2 format carries a per-kind payload
(indexing.md §Storage Layout), but the in-memory plumbing is
composite-shaped: `pinnedIndex` holds `{decl, schemaHash, root,
count}` only, and `flushIndexRegistry` / `writeNewIndexRegistry` /
the rebuild path REBUILD entries from the pinned decl — a stored
`KindPayload` would be dropped on the next flush. Unreachable
today: the open, rebuild, and drop gates all reject
non-composite stored entries and supplied decls
(`ErrIndexKindUnknown`), so no payload-carrying entry can
coexist with a flush. The first non-composite kind must
shape `pinnedIndex`, the snapshot/restore pair, and the flush
path to round-trip the payload before its entries exist.

## Provenance

Folded requirement from the format-groundwork work whose
format half landed in indexing.md §Storage Layout / §Drift
Guard; this in-memory half was deliberately not built while
unreachable.

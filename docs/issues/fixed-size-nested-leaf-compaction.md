# Fixed-size nested-tree leaves are not compact (spec'd encoding missing)

Lands: 6

## Finding

**[L→spec-required] set-keyspace.md §Fixed-Size Value Sets promises
"Compact nested B+tree leaves (no ValueLen field per cell)" for
`FixedValueSize` keyspaces; the implementation stores nested-tree
members as ordinary inline cells with a u32 ValueLen on every path**
(`internal/btree/subpage_promotion.go:115` `AddInline(v, nil)`;
`set_keyspace.go:786` `InsertIfAbsent(…, value, nil)`). Encodings are
contract; effect is ~4 wasted bytes/member and a materially earlier
promotion-overflow point. No wrong read results (encode/decode are
consistent) — a spec'd capability missing, not corruption.

## Fix direction

Implement the compact cell encoding for fixed-size nested leaves per
the spec (code conforms to spec; pre-v1 clean break — no installed
base, no format discriminator needed per breaking-changes policy;
`development: true` in `.semrel.yaml`). Land after the multi-leaf
promotion rebuild (previous chunk) since both touch the same build
path and test surface.

## Provenance

2026-07-10 defect audit; keyspace reviewer.

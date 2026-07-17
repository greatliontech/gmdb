# LeafBuilder keys-out-of-order panic on adjacent long keys

Lands: when the long-key ordering defect is diagnosed and fixed
(its own change set: diagnosis in the page/btree long-key
comparison or insertion path, fix, regression net).

Found by the rapid property sweep (`TestPropertyLongKeysAcrossSurfaces`),
seed-dependent: the builder panics "LeafBuilder keys out of order"
on two adjacent all-0x01 keys of near-equal (multi-KB) length —
the ordering the builder receives violates its precondition, so
the defect sits upstream in the long-key (overflow-key cell)
compare/sort/insert path, not in the builder's check. Pre-existing:
reproduces at 91e5c87 (before the current change sets) with the
committed reproducer `docs/issues/leafbuilder-longkey-order.fail`
(stored here, NOT under testdata/rapid/, where rapid would
auto-replay it and keep the whole suite red until the fix lands);
two older fail files (2026-07-16) in the same directory are
invalidated by generator drift but suggest the same class. Run:

    go test -run TestPropertyLongKeysAcrossSurfaces \
      -rapid.failfile=docs/issues/leafbuilder-longkey-order.fail .

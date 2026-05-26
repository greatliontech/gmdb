# MaintenanceOptions.CompactionThreshold = 0.0 (disabled) is unreachable

**Lands:** 12.5 — when incremental compaction (Task 4) is wired and
actually consumes `CompactionThreshold`; the disable semantics + default
are finalized there.

## Problem

`background-maintenance.md §Options` documents `CompactionThreshold`:
"Range: 0.0 (disabled) to 1.0. Default: 0.5." But `Options.applyDefaults`
(`options.go`) defaults it via `cmp.Or(o.Maintenance.CompactionThreshold,
0.5)`, and `cmp.Or` only replaces the *zero* value — so a user who sets
`0.0` to disable compaction gets `0.5` instead. The "disable compaction
specifically" contract (0.0) is therefore unexpressible: the zero value and
the disable sentinel collide.

`Maintenance.Disable` disables ALL maintenance, not just compaction, so it
is not a substitute.

The field is **inert until 12.5** (no code consumes `CompactionThreshold`
until incremental compaction lands), so this is latent — but the public
option ships from chunk 12.2 with a contract it does not yet meet.

## Resolution options (decide at 12.5)

1. **Sentinel:** treat a negative value (or a separate `*float64` /
   explicit-set bool) as "disabled", keep `[0,1]` as active, default
   unset → 0.5. Lets 0.0 mean a genuine never-trigger threshold distinct
   from disabled.
2. **Spec-amend:** drop the "0.0 (disabled)" clause — `0.0` becomes the
   default (always-eligible-to-trigger) and per-feature disable is only via
   `Maintenance.Disable`. Then the current `cmp.Or` default is correct and
   the spec matches.

Surfaced by the chunk-12.2 adversarial review (M-4). `class=introduced`
(the option is new in 12.2), but inert — file-and-proceed; the on-disk /
runtime behavior is unaffected until compaction lands.

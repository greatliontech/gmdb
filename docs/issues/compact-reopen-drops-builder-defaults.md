# Compact reopen drops the engine-wide builder defaults

`reopenAfterCompact` (`compact.go`) rebuilds the writer pager with
`pager.OpenParams{Pool, MaxTxBufferBytes, NoFullFsync}` only — it
omits `RestartGroupTarget`, `LeafLayout`, and `BranchLayout`, which
`Open` (`db.go`) passes from `Options`. From the Compact call until
the handle is closed and re-opened, pages are built with the engine
defaults instead of the configured ones.

Failure scenario: open with `Options.RestartGroupTarget = 1`
(uncompressed leaves), call `Compact`, insert into a keyspace whose
descriptor declares the engine default — the new leaves are built
compressed with target 6, contradicting the documented Options
semantics ("engine-wide defaults ... applied at page-build time",
`options.go`). Not corruption — per-page type-byte dispatch keeps
mixed layouts readable — but an in-spec input yielding behavior the
Options contract forbids.

Fix shape: pass the three fields in `reopenAfterCompact`'s
`OpenParams` exactly as `db.go` does, plus a regression test pinning
one observable (e.g. leaf variant under `RestartGroupTarget = 1`
after Compact).

Lands: when the post-Compact write path next changes, or as a
stand-alone burn-down item.

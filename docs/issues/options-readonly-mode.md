# Options.ReadOnly (read-only open mode) spec'd and referenced by godoc, but no field and no implementation

**Lands:** proactive — capability gap with active doc-rot (the godoc
already names a field that does not exist).

**Severity:** [M]

**Source:** 2026-05-30 deep audit (run `wf_4ad12a2f-039`), raw finding 10.

**Governing spec:** `docs/specs/mmap-strategy.md:85-90`; the live
`options.go:98` godoc already references the non-existent field.

## Problem

A documented `Open` mode — mount a DB from read-only media / enforce
no-writes — cannot be requested: the `Options.ReadOnly` field does not
exist, while the `Options` godoc (`options.go:98`) claims it does. A user
reading the godoc would expect `Options{ReadOnly: true}` to work; it
silently has no field. Untracked until now.

## Fix

Add the `Options.ReadOnly` field and implement the read-only open path
(skip writer/flock init, reject writes), **or** file a concrete deferral
and remove the dangling `ReadOnly` reference from the `options.go:98`
godoc and the `mmap-strategy.md` promise until built. Do not leave the
godoc naming a field that does not exist.

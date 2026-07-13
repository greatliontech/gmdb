# Iterators swallow cursor read errors

`Keyspace.All` / `Range` / `Prefix` (and the SetKeyspace / typed
mirrors) drive `btree.Cursor` and loop while the yielded key is
non-nil; none re-check `Cursor.Err()` per step or surface it to the
caller — `iter.Seq2` has no error channel. A mid-iteration read
failure (e.g. `ErrBadPageChecksum` on an overflow-value page) ends
the loop silently: the caller sees a clean, SHORT iteration and
cannot distinguish it from a smaller keyspace.

The value-side swallow predates overflow-key cells (overflow VALUE
assembly could always fail mid-iteration; `adoptEntry` records the
error and yields a nil value). The key-side variant introduced by
overflow-key materialization was closed at its source — a
materialization failure now yields a nil key, stopping iteration
with `Err()` set — but the stop is still silent through the
`iter.Seq2` surface.

Resolution shape: an `Err()`-style accessor on whatever handle the
iteration hangs off (the keyspace handle already carries one for
`IndexHandle`), documented as the post-iteration check; or an
error-carrying terminal yield convention. Either way the typed and
query layers need the same surface threaded through.

## Status: Open

## Severity: Medium

## Lands: when the iterator surfaces gain a documented post-iteration error check (the IndexHandle.Err precedent), or when a consumer needs to distinguish clean-end from error-end iteration

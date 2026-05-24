package gmdb

import (
	"errors"
	"fmt"
	"unique"

	"github.com/thegrumpylion/gmdb/internal/btree"
	"github.com/thegrumpylion/gmdb/internal/page"
)

// uniqueNameHandle is the interned form of a keyspace name. unique.Make
// (Go 1.23+) maps equal strings to equal handles per-process, so
// repeated lookups against the same name within a tx (or across txs)
// hit the same cache entry without per-call byte comparison. Per
// keyspaces.md §Keyspace Name Interning.
type uniqueNameHandle = unique.Handle[string]

// Keyspace is a handle to a named single-value keyspace within a write
// transaction. Returned by Tx.OpenKeyspace / Tx.CreateKeyspace /
// Tx.CreateKeyspaceIfNotExists. The chunk-5.4 surface registers and
// looks up the keyspace's descriptor in the keyspace B+tree; the
// chunk-5.5 surface adds Get / Put / Delete / Cursor on the handle.
//
// A handle is valid for the lifetime of the owning transaction. Per
// api-surface.md §Keyspace API, DeleteKeyspace (chunk 5.6) invalidates
// every handle previously opened on the named keyspace within the
// same tx; subsequent operations on those handles return
// ErrKeyspaceClosed (sentinel defined alongside the DeleteKeyspace
// implementation).
type Keyspace struct {
	tx   *Tx
	name uniqueNameHandle

	// desc is the in-tx view of the keyspace's descriptor. Mutated
	// in place by Put / Delete data-op paths (descriptor.Root +
	// descriptor.Count flow back into the keyspace B+tree via a
	// btree.Put on every committed change).
	desc page.KeyspaceDescriptor

	// openCursors tracks every *Cursor returned by Keyspace.Cursor()
	// in this tx so Put / Delete can MarkStale them — sibling
	// mutations to the keyspace's B+tree invalidate cursor state
	// (curKey / iter alias leaf-buffer slices that the mutation may
	// CoW or free). Per cursor-markstale-clear-cur.md.
	openCursors []*Cursor
}

// Name returns the keyspace's name (the unique-interned identity).
// Allocations: returns the underlying string from the interned handle
// without copying.
func (ks *Keyspace) Name() string { return ks.name.Value() }

// OpenKeyspace opens an existing single-value keyspace (Kind=0) for
// read+write. Returns ErrNotFound if the named keyspace does not
// exist; ErrKeyspaceKindMismatch if the stored descriptor's Kind is 1
// (SetKeyspace — use OpenSetKeyspace in chunk 6); ErrKeyspaceReserved
// if the name resolves to an engine-internal keyspace (Kind=2);
// ErrCorrupted (wrapping the codec validate error) if the descriptor
// fails ValidateKeyspaceDescriptor (unknown Kind, FixedValueSize set
// on Kind != 1, non-zero reserved bytes, RestartGroupTarget > 255).
//
// Signature deferral: api-surface.md specifies
// `OpenKeyspace(name string, indexes ...*IndexDecl)`. The variadic
// IndexDecls land at chunk 7 alongside the indexing implementation;
// adding a variadic argument is a non-breaking Go-language extension,
// so chunk-5.4 callers do not need source changes when chunk 7 lands.
func (tx *Tx) OpenKeyspace(name string) (*Keyspace, error) {
	if err := tx.requireOpen(true); err != nil {
		return nil, err
	}
	if name == "" {
		return nil, ErrKeyEmpty
	}
	handle := unique.Make(name)
	if ks, ok := tx.openKeyspaces[handle]; ok {
		return ks, nil
	}
	desc, found, err := tx.loadDescriptor(name)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, ErrNotFound
	}
	if err := checkKeyspaceKind(desc.Kind, page.KeyspaceKindKeyspace); err != nil {
		return nil, err
	}
	return tx.cacheOpenKeyspace(handle, desc), nil
}

// CreateKeyspace creates a new single-value keyspace (Kind=0). Returns
// ErrKeyExists if a keyspace with the supplied name already exists
// (use CreateKeyspaceIfNotExists for the open-or-create shape).
//
// The keyspace descriptor lands as a 40-byte value in the keyspace
// B+tree under the name's UTF-8 bytes as key. The keyspace B+tree's
// new root + incremented numKeyspaces are tracked in tx state and
// propagate to the next meta on Commit.
//
// Chunk 5.4 does not yet accept IndexDecls — that surface lands at
// chunk 7.
func (tx *Tx) CreateKeyspace(name string) (*Keyspace, error) {
	if err := tx.requireOpen(true); err != nil {
		return nil, err
	}
	if name == "" {
		return nil, ErrKeyEmpty
	}
	handle := unique.Make(name)
	if _, ok := tx.openKeyspaces[handle]; ok {
		// Already opened in this tx — caller is racing themselves;
		// surface the same ErrKeyExists they'd get from a fresh
		// CreateKeyspace.
		return nil, ErrKeyExists
	}
	if _, found, err := tx.loadDescriptor(name); err != nil {
		return nil, err
	} else if found {
		return nil, ErrKeyExists
	}
	desc := page.KeyspaceDescriptor{
		Root:               0,
		Count:              0,
		Kind:               page.KeyspaceKindKeyspace,
		FixedValueSize:     0,
		NextSeq:            0,
		RestartGroupTarget: 0,
		IndexRegistryRoot:  0,
	}
	if err := tx.storeDescriptor(name, desc); err != nil {
		return nil, err
	}
	tx.numKeyspaces++
	return tx.cacheOpenKeyspace(handle, desc), nil
}

// CreateKeyspaceIfNotExists opens the keyspace if it exists (with the
// matching Kind=0 check) or creates it. The chunk-7 IndexDecl-matching
// behaviour lands alongside indexing — this chunk-5.4 surface accepts
// no index declarations.
func (tx *Tx) CreateKeyspaceIfNotExists(name string) (*Keyspace, error) {
	if err := tx.requireOpen(true); err != nil {
		return nil, err
	}
	if name == "" {
		return nil, ErrKeyEmpty
	}
	handle := unique.Make(name)
	if ks, ok := tx.openKeyspaces[handle]; ok {
		return ks, nil
	}
	desc, found, err := tx.loadDescriptor(name)
	if err != nil {
		return nil, err
	}
	if found {
		if err := checkKeyspaceKind(desc.Kind, page.KeyspaceKindKeyspace); err != nil {
			return nil, err
		}
		return tx.cacheOpenKeyspace(handle, desc), nil
	}
	// Not found — create.
	desc = page.KeyspaceDescriptor{Kind: page.KeyspaceKindKeyspace}
	if err := tx.storeDescriptor(name, desc); err != nil {
		return nil, err
	}
	tx.numKeyspaces++
	return tx.cacheOpenKeyspace(handle, desc), nil
}

// ListKeyspaces returns the names of all user keyspaces (Kind=0 or
// Kind=1). Engine-internal index keyspaces (Kind=2) are filtered out
// per keyspaces.md invariant #4 — they are addressable only via their
// parent keyspace's index registry, not by name. Names are returned
// in sorted byte order (the keyspace B+tree's natural order).
//
// Iteration uses a chunk-4 cursor against the keyspace B+tree's
// current in-tx root. A keyspace created earlier in this tx is
// included; one deleted in this tx (chunk 5.6) is excluded.
func (tx *Tx) ListKeyspaces() ([]string, error) {
	if err := tx.requireOpen(false); err != nil {
		return nil, err
	}
	if tx.keyspaceRoot == 0 {
		return nil, nil
	}
	cfg := tx.pgr.Config()
	c := btree.NewReadCursor(tx.pgr, cfg, tx.keyspaceRoot)
	var names []string
	for k, v := c.First(); k != nil; k, v = c.Next() {
		if len(v) != page.KeyspaceDescriptorSize {
			return nil, fmt.Errorf("%w: keyspace descriptor value length %d != %d",
				ErrCorrupted, len(v), page.KeyspaceDescriptorSize)
		}
		desc := page.DecodeKeyspaceDescriptor(v)
		if err := page.ValidateKeyspaceDescriptor(v, desc); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrCorrupted, err)
		}
		if desc.Kind == page.KeyspaceKindIndexInternal {
			continue
		}
		names = append(names, string(k))
	}
	if err := c.Err(); err != nil {
		return nil, mapBtreeErr(err)
	}
	return names, nil
}

// loadDescriptor reads the descriptor for name from the keyspace
// B+tree. Returns (desc, true, nil) on hit; (zero, false, nil) when
// the name is absent; (zero, false, err) on btree/codec failure.
func (tx *Tx) loadDescriptor(name string) (page.KeyspaceDescriptor, bool, error) {
	if tx.keyspaceRoot == 0 {
		return page.KeyspaceDescriptor{}, false, nil
	}
	cfg := tx.pgr.Config()
	value, found, err := btree.Get(tx.pgr, cfg, tx.keyspaceRoot, []byte(name))
	if err != nil {
		return page.KeyspaceDescriptor{}, false, mapBtreeErr(err)
	}
	if !found {
		return page.KeyspaceDescriptor{}, false, nil
	}
	if len(value) != page.KeyspaceDescriptorSize {
		return page.KeyspaceDescriptor{}, false, fmt.Errorf("%w: keyspace descriptor value length %d != %d",
			ErrCorrupted, len(value), page.KeyspaceDescriptorSize)
	}
	desc := page.DecodeKeyspaceDescriptor(value)
	if err := page.ValidateKeyspaceDescriptor(value, desc); err != nil {
		return page.KeyspaceDescriptor{}, false, fmt.Errorf("%w: %v", ErrCorrupted, err)
	}
	return desc, true, nil
}

// storeDescriptor encodes desc and writes it into the keyspace B+tree
// under name's UTF-8 bytes as the key. Mutates tx.keyspaceRoot to the
// new root returned by btree.Put. Caller increments numKeyspaces
// after success.
func (tx *Tx) storeDescriptor(name string, desc page.KeyspaceDescriptor) error {
	buf := make([]byte, page.KeyspaceDescriptorSize)
	page.EncodeKeyspaceDescriptor(buf, desc)
	cfg := tx.pgr.Config()
	newRoot, err := btree.Put(tx.pgr, cfg, tx.keyspaceRoot, []byte(name), buf)
	if err != nil {
		return mapBtreeErr(err)
	}
	tx.keyspaceRoot = newRoot
	return nil
}

// cacheOpenKeyspace constructs the *Keyspace and registers it in the
// tx's per-name cache. Both the new-Create and the existing-Open
// paths route through here.
func (tx *Tx) cacheOpenKeyspace(handle uniqueNameHandle, desc page.KeyspaceDescriptor) *Keyspace {
	ks := &Keyspace{tx: tx, name: handle, desc: desc}
	if tx.openKeyspaces == nil {
		tx.openKeyspaces = make(map[uniqueNameHandle]*Keyspace)
	}
	tx.openKeyspaces[handle] = ks
	return ks
}

// checkKeyspaceKind verifies the stored Kind matches the API used.
// Kind=2 (engine-internal) routes to ErrKeyspaceReserved per
// keyspaces.md invariant #4; mismatched user Kinds (Kind=0 vs
// Kind=1) route to ErrKeyspaceKindMismatch per invariant #3.
func checkKeyspaceKind(stored, requested uint8) error {
	if stored == requested {
		return nil
	}
	if stored == page.KeyspaceKindIndexInternal {
		return ErrKeyspaceReserved
	}
	return ErrKeyspaceKindMismatch
}

// builderCfg returns the page.Config to pass to btree.* calls for
// this keyspace. When the per-keyspace RestartGroupTarget is set
// (via SetKeyspaceConfig per keyspaces.md invariant #6), it
// overrides the engine default — newly written leaves use the
// per-keyspace target. Decoding ignores RestartGroupTarget so the
// override is safe on Get-side too.
func (ks *Keyspace) builderCfg() page.Config {
	cfg := ks.tx.pgr.Config()
	if ks.desc.RestartGroupTarget != 0 {
		cfg.RestartGroupTarget = ks.desc.RestartGroupTarget
	}
	return cfg
}

// Get returns the value stored under key in the keyspace. Returns
// (nil, ErrNotFound) if the key is absent. Returns ([]byte{}, nil)
// for a key whose stored value is empty. Per api-surface.md
// §Invariants, the returned slice is a borrowed reference valid
// until transaction close. ErrKeyEmpty if key is nil or empty.
func (ks *Keyspace) Get(key []byte) ([]byte, error) {
	if err := ks.tx.requireOpen(false); err != nil {
		return nil, err
	}
	if len(key) == 0 {
		return nil, ErrKeyEmpty
	}
	if ks.desc.Root == 0 {
		return nil, ErrNotFound
	}
	cfg := ks.builderCfg()
	value, found, err := btree.Get(ks.tx.pgr, cfg, ks.desc.Root, key)
	if err != nil {
		return nil, mapBtreeErr(err)
	}
	if !found {
		return nil, ErrNotFound
	}
	return value, nil
}

// Put inserts or replaces (key, value) in the keyspace. nil values are
// treated as empty (api-surface.md §Invariants — nil-value-as-empty).
// ErrKeyEmpty if key is nil or empty. Other errors map from the btree
// layer (ErrKeyTooLarge for oversize keys, ErrTxTooLarge for slab-
// budget exhaustion).
//
// Side effects on success:
//   - descriptor.Root is updated to the new btree root.
//   - descriptor.Count is incremented iff the key did not previously
//     exist in the keyspace.
//   - The updated descriptor is re-encoded and written back into the
//     keyspace B+tree (which CoWs through to tx.keyspaceRoot and on
//     to meta.KeyspaceRoot at commit).
//   - Every open Cursor on this keyspace is MarkStale'd — subsequent
//     non-repositioning cursor ops surface ErrCursorStale.
func (ks *Keyspace) Put(key, value []byte) error {
	if err := ks.tx.requireOpen(true); err != nil {
		return err
	}
	if len(key) == 0 {
		return ErrKeyEmpty
	}
	cfg := ks.builderCfg()
	if value == nil {
		value = []byte{}
	}
	// Existence check so we know whether to increment Count.
	existed := false
	if ks.desc.Root != 0 {
		exists, err := btree.Has(ks.tx.pgr, cfg, ks.desc.Root, key)
		if err != nil {
			return mapBtreeErr(err)
		}
		existed = exists
	}
	newRoot, err := btree.Put(ks.tx.pgr, cfg, ks.desc.Root, key, value)
	if err != nil {
		return mapBtreeErr(err)
	}
	ks.desc.Root = newRoot
	if !existed {
		ks.desc.Count++
	}
	if err := ks.tx.storeDescriptor(ks.name.Value(), ks.desc); err != nil {
		return err
	}
	ks.markCursorsStale()
	return nil
}

// Delete removes the entry under key. Returns ErrNotFound if the key
// does not exist (api-surface.md §Invariants — keyed-removal returns
// ErrNotFound on miss; chunk-5.1 user-locked decision). ErrKeyEmpty
// if key is nil or empty.
//
// Side effects on success:
//   - descriptor.Root reflects the new btree root (0 when the
//     keyspace is emptied).
//   - descriptor.Count is decremented.
//   - Descriptor written back into the keyspace B+tree.
//   - Every open Cursor on this keyspace is MarkStale'd.
func (ks *Keyspace) Delete(key []byte) error {
	if err := ks.tx.requireOpen(true); err != nil {
		return err
	}
	if len(key) == 0 {
		return ErrKeyEmpty
	}
	if ks.desc.Root == 0 {
		return ErrNotFound
	}
	cfg := ks.builderCfg()
	mergeThreshold := ks.tx.db.opts.MergeThreshold
	newRoot, err := btree.Delete(ks.tx.pgr, cfg, ks.desc.Root, mergeThreshold, key)
	if err != nil {
		if errors.Is(err, btree.ErrNotFound) {
			return ErrNotFound
		}
		return mapBtreeErr(err)
	}
	ks.desc.Root = newRoot
	ks.desc.Count--
	if err := ks.tx.storeDescriptor(ks.name.Value(), ks.desc); err != nil {
		return err
	}
	ks.markCursorsStale()
	return nil
}

// Cursor returns a new cursor for iterating over this keyspace's
// (key, value) pairs. The cursor starts Unpositioned — call First /
// Last / Seek / SeekGE before reading. Per transactions.md §Cursor
// State Machine.
//
// Sibling-mutation contract: Keyspace.Put / Delete on this keyspace
// MarkStale's every Cursor returned by this method that is still
// reachable; subsequent non-repositioning cursor ops surface
// ErrCursorStale. The caller re-positions via First / Last / Seek /
// SeekGE and continues.
func (ks *Keyspace) Cursor() *Cursor {
	cfg := ks.builderCfg()
	var inner *btree.Cursor
	if ks.tx.writable {
		inner = btree.NewCursor(ks.tx.pgr, cfg, ks.desc.Root, ks.tx.db.opts.MergeThreshold)
	} else {
		inner = btree.NewReadCursor(ks.tx.pgr, cfg, ks.desc.Root)
	}
	c := &Cursor{inner: inner, tx: ks.tx, ks: ks}
	ks.openCursors = append(ks.openCursors, c)
	return c
}

// markCursorsStale invokes MarkStale on every cursor registered on
// this keyspace. Called by Put / Delete after a successful mutation.
// Stale cursors are not unregistered — the caller may re-position
// them via First/Last/Seek/SeekGE without needing a fresh
// Keyspace.Cursor() call.
func (ks *Keyspace) markCursorsStale() {
	for _, c := range ks.openCursors {
		c.inner.MarkStale()
	}
}

// KeyspaceConfig is the mutable per-keyspace configuration surface
// for Tx.SetKeyspaceConfig.
type KeyspaceConfig struct {
	// RestartGroupTarget sets the leaf-builder restart-group target
	// for leaves written AFTER this call (existing leaves keep
	// their stored group structure per keyspaces.md invariant #6).
	// 0 = leave unchanged (the sentinel distinct from the on-disk
	// descriptor's "0 = engine default" semantic).
	// [1, 255] = the new builder hint.
	// > 255 returns ErrInvalidOptions.
	RestartGroupTarget uint16
}

// SetKeyspaceConfig updates mutable per-keyspace settings on the named
// keyspace. Kind-agnostic at the descriptor layer (the descriptor's
// RestartGroupTarget field lives independently of Kind). Returns:
//
//   - ErrNotFound if the named keyspace does not exist (resolved
//     per the chunk-5.4-filed
//     docs/issues/tx-setkeyspaceconfig-missing-name-behavior.md —
//     consistent with Tx.DeleteKeyspace and the Delete-on-miss
//     invariant family).
//   - ErrKeyEmpty for an empty name.
//   - ErrInvalidOptions when cfg.RestartGroupTarget > 255.
//
// cfg.RestartGroupTarget == 0 is the "leave unchanged" sentinel — the
// existing descriptor's RestartGroupTarget is preserved. A no-op call
// (every field zero) is harmless and does not write the descriptor.
func (tx *Tx) SetKeyspaceConfig(name string, cfg KeyspaceConfig) error {
	if err := tx.requireOpen(true); err != nil {
		return err
	}
	if name == "" {
		return ErrKeyEmpty
	}
	if cfg.RestartGroupTarget > page.MaxRestartGroupTarget {
		return fmt.Errorf("%w: RestartGroupTarget %d exceeds max %d",
			ErrInvalidOptions, cfg.RestartGroupTarget, page.MaxRestartGroupTarget)
	}
	desc, found, err := tx.loadDescriptor(name)
	if err != nil {
		return err
	}
	if !found {
		return ErrNotFound
	}
	// 0 = leave unchanged. No other fields are configurable today.
	if cfg.RestartGroupTarget == 0 {
		return nil
	}
	if desc.RestartGroupTarget == cfg.RestartGroupTarget {
		return nil
	}
	desc.RestartGroupTarget = cfg.RestartGroupTarget
	if err := tx.storeDescriptor(name, desc); err != nil {
		return err
	}
	// Refresh any cached *Keyspace handle so a subsequent OpenKeyspace
	// (which hits the cache first) sees the new RestartGroupTarget.
	if ks, ok := tx.openKeyspaces[unique.Make(name)]; ok {
		ks.desc.RestartGroupTarget = cfg.RestartGroupTarget
	}
	return nil
}

// Cursor is a public read-and-(when on a write tx)-mutate cursor over
// a single keyspace's (key, value) pairs. State-machine semantics per
// transactions.md §Cursor State Machine: starts Unpositioned;
// First/Last/Seek/SeekGE transition to Positioned or End-of-iteration;
// Next/Prev advance; Delete (on a write tx) removes the current entry
// and advances to the post-delete successor (or End-of-iteration).
//
// Stale handling: a sibling Put/Delete on the same keyspace
// invalidates this cursor — Current/Next/Prev/Delete surface
// ErrCursorStale until the caller re-positions.
//
// Tx lifecycle: every navigation/mutation method first checks that
// the parent tx is still open. After tx.Commit() or tx.Rollback() the
// cursor returns (nil, nil) and Err() reports ErrTxClosed — guarding
// against reads of pool-recycled slab buffers or post-Close munmap'd
// mmap pages.
type Cursor struct {
	inner    *btree.Cursor
	tx       *Tx
	ks       *Keyspace
	closeErr error
}

func (c *Cursor) requireOpen(needsWrite bool) bool {
	if c.closeErr != nil {
		return false
	}
	if err := c.tx.requireOpen(needsWrite); err != nil {
		c.closeErr = err
		return false
	}
	return true
}

// First positions the cursor at the leftmost entry. Returns
// (nil, nil) on an empty keyspace.
func (c *Cursor) First() (key, value []byte) {
	if !c.requireOpen(false) {
		return nil, nil
	}
	return c.inner.First()
}

// Last positions at the rightmost entry. Returns (nil, nil) on empty.
func (c *Cursor) Last() (key, value []byte) {
	if !c.requireOpen(false) {
		return nil, nil
	}
	return c.inner.Last()
}

// Next advances by one entry. Unpositioned ⇒ First. End-of-iteration
// ⇒ (nil, nil). Sets Err to ErrCursorStale when a sibling mutation
// has invalidated the cursor.
func (c *Cursor) Next() (key, value []byte) {
	if !c.requireOpen(false) {
		return nil, nil
	}
	return c.inner.Next()
}

// Prev steps backward by one entry. Unpositioned ⇒ Last.
func (c *Cursor) Prev() (key, value []byte) {
	if !c.requireOpen(false) {
		return nil, nil
	}
	return c.inner.Prev()
}

// Seek positions at the exact key. On miss returns (nil, nil) with
// End-of-iteration state and Err == nil.
func (c *Cursor) Seek(target []byte) (key, value []byte) {
	if !c.requireOpen(false) {
		return nil, nil
	}
	return c.inner.Seek(target)
}

// SeekGE positions at the smallest key >= target. Returns (nil, nil)
// when no such key exists.
func (c *Cursor) SeekGE(target []byte) (key, value []byte) {
	if !c.requireOpen(false) {
		return nil, nil
	}
	return c.inner.SeekGE(target)
}

// Current returns the current (key, value) without advancing.
// (nil, nil) at End-of-iteration or Unpositioned.
func (c *Cursor) Current() (key, value []byte) {
	if !c.requireOpen(false) {
		return nil, nil
	}
	return c.inner.Current()
}

// Delete removes the current entry. Cursor must be Positioned;
// otherwise returns ErrCursorUnpositioned. After delete, advances to
// the next entry or transitions to End-of-iteration. On a read-only
// transaction returns ErrReadOnly.
func (c *Cursor) Delete() error {
	if !c.requireOpen(true) {
		return c.closeErr
	}
	if err := c.inner.Delete(); err != nil {
		if errors.Is(err, btree.ErrCursorUnpositioned) {
			return ErrCursorUnpositioned
		}
		if errors.Is(err, btree.ErrReadOnly) {
			return ErrReadOnly
		}
		if errors.Is(err, btree.ErrCursorStale) {
			return ErrCursorStale
		}
		return mapBtreeErr(err)
	}
	// Cursor.Delete mutated the keyspace's B+tree. Update the
	// descriptor and re-encode it (mirrors Keyspace.Delete's
	// post-conditions). Then mark sibling cursors stale.
	c.ks.desc.Root = c.inner.RootID()
	c.ks.desc.Count--
	if err := c.tx.storeDescriptor(c.ks.name.Value(), c.ks.desc); err != nil {
		return err
	}
	// Mark stale on every OTHER cursor (this cursor self-recovered
	// via its internal SeekGE in btree.Cursor.Delete).
	for _, sibling := range c.ks.openCursors {
		if sibling != c {
			sibling.inner.MarkStale()
		}
	}
	return nil
}

// Err returns the sticky error captured by the most recent
// repositioning op (First / Last / Next / Prev / Seek / SeekGE)
// or by Delete. Includes ErrCursorStale on sibling-mutation
// invalidation and ErrTxClosed when the parent tx has closed.
func (c *Cursor) Err() error {
	if c.closeErr != nil {
		return c.closeErr
	}
	if err := c.inner.Err(); err != nil {
		if errors.Is(err, btree.ErrCursorStale) {
			return ErrCursorStale
		}
		if errors.Is(err, btree.ErrCorrupted) {
			return fmt.Errorf("%w: %v", ErrCorrupted, err)
		}
		return err
	}
	return nil
}

// mapBtreeErr translates internal btree sentinels into public gmdb
// sentinels. btree.ErrCorrupted → ErrCorrupted; other btree errors
// pass through unwrapped.
//
// Chunk-5.4 caveat: btree.ErrKeyTooLarge is reachable through
// storeDescriptor if a future caller supplies a pathologically long
// keyspace name. Chunk 5.4 does not impose a name-length cap (api-
// surface.md is silent on it). Chunk 5.5's data-op surface should add
// the cap and the public ErrKeyTooLarge translation before exposing
// user-controlled keys via Keyspace.Put.
func mapBtreeErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, btree.ErrCorrupted) {
		return fmt.Errorf("%w: %v", ErrCorrupted, err)
	}
	return err
}

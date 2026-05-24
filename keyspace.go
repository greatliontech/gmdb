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
	// via the chunk-5.5 Put / Delete data-op paths (descriptor.Root
	// + descriptor.Count flow back into the keyspace B+tree via a
	// btree.Put on every committed change). At chunk 5.4 the
	// descriptor is read-only on the handle — only Open / Create
	// populates it.
	desc page.KeyspaceDescriptor
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

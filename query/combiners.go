package query

import (
	"iter"
	"strings"

	"github.com/greatliontech/gmdb"
)

// Union and Intersect execution (query-builder.md §Plan nodes).
// Both pre-validate every leaf's LIVE declaration and degrade the
// whole plan to a scan on any tuple change — the Or (and every
// other term) is always evaluable residually, so the scan is
// correct under any declaration shape (Inv-QB1/Inv-QB3).

// branchHandle is one validated leaf handle pair.
type branchHandle struct {
	idx *gmdb.IndexHandle
	d   *gmdb.IndexDecl
}

// openBranches obtains a fresh handle per leaf and validates the
// live tuples. ok=false → the caller falls back to a scan; an
// acquisition error is on q.err.
func (q *Query[K, V]) openBranches(leaves []plannedLeaf) ([]branchHandle, bool) {
	hs := make([]branchHandle, len(leaves))
	for i, leaf := range leaves {
		idx, err := q.ks.ByteIndex(leaf.index.Name)
		if err != nil {
			q.err = err
			return nil, false
		}
		d := idx.Decl()
		if !liveColumnsMatch(leaf.index, d) {
			return nil, false
		}
		hs[i] = branchHandle{idx: idx, d: d}
	}
	return hs, true
}

// rowKV packs one branch row for the merge machinery.
type rowKV[K, V any] struct {
	k K
	v V
}

// unionDrive drains a RESOLVED rule-4 Union: each branch's leaf
// rows (group residuals applied inside the branch) dedup by PK
// across branches (Inv-QB4), then flow through the caller's tail.
// The merge arm runs a k-way minimum merge over the branches'
// PK-ascending streams — duplicates meet at the merge point; the
// hash arm drains branches in plan order against a seen-PK set
// (deterministic, not canonical — Inv-QB5's no-OrderBy regime),
// charging the set against the materialization budget (Inv-QB6).
func (q *Query[K, V]) unionDrive(p queryPlan, hs []branchHandle, m *meter, tail func(K, V) bool) {
	if p.merge {
		q.unionMerge(p, hs, m, tail)
		return
	}
	seen := make(map[string]struct{})
	for i, b := range p.branches {
		stopped := false
		q.leafDrive(hs[i].idx, hs[i].d, b.leaf, b.resid, false, m, func(pk string, k K, v V) bool {
			if _, dup := seen[pk]; dup {
				return true
			}
			if !m.charge(len(pk)) {
				q.err = ErrQueryMaterializeLimit
				stopped = true
				return false
			}
			seen[pk] = struct{}{}
			if !tail(k, v) {
				stopped = true
				return false
			}
			return true
		})
		if stopped || q.err != nil {
			return
		}
	}
}

// unionMerge is the streaming arm: every branch is an IndexSeek
// (PK-ascending, one entry per row), so equal PKs from different
// branches surface consecutively at the minimum-selection point
// and dedup without buffering.
func (q *Query[K, V]) unionMerge(p queryPlan, hs []branchHandle, m *meter, tail func(K, V) bool) {
	type head struct {
		pk string
		kv rowKV[K, V]
		ok bool
	}
	n := len(p.branches)
	nexts := make([]func() (string, rowKV[K, V], bool), n)
	stops := make([]func(), n)
	for i := range p.branches {
		b := p.branches[i]
		h := hs[i]
		seq := func(y func(string, rowKV[K, V]) bool) {
			q.leafDrive(h.idx, h.d, b.leaf, b.resid, false, m, func(pk string, k K, v V) bool {
				return y(pk, rowKV[K, V]{k: k, v: v})
			})
		}
		nexts[i], stops[i] = iter.Pull2(iter.Seq2[string, rowKV[K, V]](seq))
		defer stops[i]()
	}
	heads := make([]head, n)
	for i := range heads {
		pk, kv, ok := nexts[i]()
		heads[i] = head{pk: pk, kv: kv, ok: ok}
		if q.err != nil {
			return
		}
	}
	last := ""
	haveLast := false
	for {
		min := -1
		for i := range heads {
			if !heads[i].ok {
				continue
			}
			if min < 0 || strings.Compare(heads[i].pk, heads[min].pk) < 0 {
				min = i
			}
		}
		if min < 0 {
			return
		}
		h := heads[min]
		if !haveLast || h.pk != last {
			if !tail(h.kv.k, h.kv.v) {
				return
			}
			last, haveLast = h.pk, true
		}
		pk, kv, ok := nexts[min]()
		heads[min] = head{pk: pk, kv: kv, ok: ok}
		if q.err != nil {
			return
		}
	}
}

// intersectDrive drains a RESOLVED rule-5 Intersect: the build
// seek materializes its PK set (charged against the
// materialization budget — Inv-QB6), the probe seek streams and
// keeps PKs present in it (the probe's ordering is preserved);
// the caller's tail applies residuals, filters, and bounds.
func (q *Query[K, V]) intersectDrive(p queryPlan, hs []branchHandle, m *meter, tail func(K, V) bool) {
	buildSet := make(map[string]struct{})
	q.leafDrive(hs[1].idx, hs[1].d, p.build, nil, false, m, func(pk string, _ K, _ V) bool {
		if !m.charge(len(pk)) {
			q.err = ErrQueryMaterializeLimit
			return false
		}
		buildSet[pk] = struct{}{}
		return true
	})
	if q.err != nil {
		return
	}
	q.leafDrive(hs[0].idx, hs[0].d, p.probe, nil, false, m, func(pk string, k K, v V) bool {
		if _, ok := buildSet[pk]; !ok {
			return true
		}
		return tail(k, v)
	})
}

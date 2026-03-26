package page

import (
	"bytes"
	"fmt"
	"testing"
)

func TestLeafInlineRoundTrip(t *testing.T) {
	cfg := PageConfig{PageSize: 4096, PageChecksum: false}
	buf := make([]byte, cfg.PageSize)

	b := NewLeafBuilder(buf, cfg)

	type kv struct {
		key, value string
	}
	entries := []kv{
		{"user:alice:email", "alice@example.com"},
		{"user:alice:name", "Alice"},
		{"user:alice:role", "admin"},
		{"user:bob:email", "bob@example.com"},
		{"user:bob:name", "Bob"},
		{"user:bob:role", "user"},
	}

	for _, e := range entries {
		if !b.AddInline([]byte(e.key), []byte(e.value)) {
			t.Fatalf("AddInline(%q) failed", e.key)
		}
	}
	count := b.Finish()
	if int(count) != len(entries) {
		t.Fatalf("Finish() = %d, want %d", count, len(entries))
	}

	// Read back.
	r := NewLeafReader(buf, cfg)
	if r.Count() != len(entries) {
		t.Fatalf("Count() = %d, want %d", r.Count(), len(entries))
	}
	if r.RestartInterval() != restartInterval {
		t.Errorf("RestartInterval() = %d, want %d", r.RestartInterval(), restartInterval)
	}

	var keyBuf []byte
	for i, want := range entries {
		e, kb := r.EntryAt(i, keyBuf)
		keyBuf = kb
		if !bytes.Equal(e.Key, []byte(want.key)) {
			t.Errorf("entry %d: key = %q, want %q", i, e.Key, want.key)
		}
		if !bytes.Equal(e.Value, []byte(want.value)) {
			t.Errorf("entry %d: value = %q, want %q", i, e.Value, want.value)
		}
		if e.CellFlags != 0 {
			t.Errorf("entry %d: CellFlags = %d, want 0", i, e.CellFlags)
		}
	}
}

func TestLeafPrefixCompression(t *testing.T) {
	cfg := PageConfig{PageSize: 4096, PageChecksum: false}
	buf := make([]byte, cfg.PageSize)

	// Generate keys with long shared prefix.
	prefix := "com.example.service.module.component."
	var keys []string
	for i := range 20 { // spans multiple restart groups
		keys = append(keys, fmt.Sprintf("%skey-%04d", prefix, i))
	}

	b := NewLeafBuilder(buf, cfg)
	for _, k := range keys {
		if !b.AddInline([]byte(k), []byte("v")) {
			t.Fatalf("AddInline(%q) failed", k)
		}
	}
	b.Finish()

	r := NewLeafReader(buf, cfg)
	if r.Count() != len(keys) {
		t.Fatalf("Count() = %d, want %d", r.Count(), len(keys))
	}
	// Verify restart count: 20 entries / 16 interval = 2 restart points.
	if r.RestartCount() != 2 {
		t.Errorf("RestartCount() = %d, want 2", r.RestartCount())
	}

	// Verify all keys decode correctly via Iter.
	idx := 0
	it := r.Iter(nil)
	for e, ok := it.Next(); ok; e, ok = it.Next() {
		if !bytes.Equal(e.Key, []byte(keys[idx])) {
			t.Errorf("entry %d: key = %q, want %q", idx, e.Key, keys[idx])
		}
		idx++
	}
}

func TestLeafSearchInline(t *testing.T) {
	cfg := PageConfig{PageSize: 4096, PageChecksum: false}
	buf := make([]byte, cfg.PageSize)

	keys := []string{
		"aaa", "bbb", "ccc", "ddd", "eee",
		"fff", "ggg", "hhh", "iii", "jjj",
		"kkk", "lll", "mmm", "nnn", "ooo",
		"ppp", "qqq", "rrr", // 18 entries = 2 restart groups
	}
	b := NewLeafBuilder(buf, cfg)
	for _, k := range keys {
		b.AddInline([]byte(k), []byte("val"))
	}
	b.Finish()

	r := NewLeafReader(buf, cfg)
	keyBuf := make([]byte, 0, 64)

	// Exact matches.
	for i, k := range keys {
		idx, entry, found := r.SearchLeaf([]byte(k), keyBuf)
		if !found {
			t.Errorf("SearchLeaf(%q): not found", k)
			continue
		}
		if idx != i {
			t.Errorf("SearchLeaf(%q): index = %d, want %d", k, idx, i)
		}
		if !bytes.Equal(entry.Key, []byte(k)) {
			t.Errorf("SearchLeaf(%q): key = %q", k, entry.Key)
		}
	}

	// Not found — before first.
	idx, _, found := r.SearchLeaf([]byte("000"), keyBuf)
	if found {
		t.Error("SearchLeaf(000): unexpectedly found")
	}
	if idx != 0 {
		t.Errorf("SearchLeaf(000): index = %d, want 0", idx)
	}

	// Not found — between entries.
	idx, _, found = r.SearchLeaf([]byte("bbc"), keyBuf)
	if found {
		t.Error("SearchLeaf(bbc): unexpectedly found")
	}
	if idx != 2 {
		t.Errorf("SearchLeaf(bbc): index = %d, want 2", idx)
	}

	// Not found — after last.
	idx, _, found = r.SearchLeaf([]byte("zzz"), keyBuf)
	if found {
		t.Error("SearchLeaf(zzz): unexpectedly found")
	}
	if idx != len(keys) {
		t.Errorf("SearchLeaf(zzz): index = %d, want %d", idx, len(keys))
	}
}

func TestLeafOverflowEntry(t *testing.T) {
	cfg := PageConfig{PageSize: 4096, PageChecksum: false}
	buf := make([]byte, cfg.PageSize)

	b := NewLeafBuilder(buf, cfg)
	b.AddInline([]byte("key1"), []byte("small"))
	b.AddOverflow([]byte("key2"), 42, 1048576)
	b.AddInline([]byte("key3"), []byte("also small"))
	b.Finish()

	r := NewLeafReader(buf, cfg)
	if r.Count() != 3 {
		t.Fatalf("Count() = %d, want 3", r.Count())
	}

	keyBuf := make([]byte, 0, 64)
	e, keyBuf := r.EntryAt(1, keyBuf)
	if !bytes.Equal(e.Key, []byte("key2")) {
		t.Errorf("key = %q, want %q", e.Key, "key2")
	}
	if e.CellFlags&CellFlagOverflow == 0 {
		t.Error("expected CellFlagOverflow set")
	}
	if e.OvflPage != 42 {
		t.Errorf("OvflPage = %d, want 42", e.OvflPage)
	}
	if e.TotalLen != 1048576 {
		t.Errorf("TotalLen = %d, want 1048576", e.TotalLen)
	}
}

func TestLeafSubpageEntry(t *testing.T) {
	cfg := PageConfig{PageSize: 4096, PageChecksum: false}
	buf := make([]byte, cfg.PageSize)

	// Build a subpage.
	spBuf := make([]byte, 128)
	spb := NewSubpageBuilder(spBuf, 0)
	spb.AddValue([]byte("val1"))
	spb.AddValue([]byte("val2"))
	spSize := spb.Finish()
	subpageData := spBuf[:spSize]

	b := NewLeafBuilder(buf, cfg)
	b.AddSubpage([]byte("setkey"), subpageData)
	b.Finish()

	r := NewLeafReader(buf, cfg)
	keyBuf := make([]byte, 0, 64)
	e, _ := r.EntryAt(0, keyBuf)

	if !bytes.Equal(e.Key, []byte("setkey")) {
		t.Errorf("key = %q, want %q", e.Key, "setkey")
	}
	if e.CellFlags&CellFlagMultiValue == 0 {
		t.Error("expected CellFlagMultiValue set")
	}
	if e.CellFlags&CellFlagNestedTree != 0 {
		t.Error("expected CellFlagNestedTree clear")
	}
	if !bytes.Equal(e.SubpageData, subpageData) {
		t.Error("SubpageData mismatch")
	}

	// Parse subpage.
	sr := NewSubpageReader(e.SubpageData, 0)
	if sr.Count() != 2 {
		t.Fatalf("subpage Count = %d, want 2", sr.Count())
	}
	if !bytes.Equal(sr.Value(0), []byte("val1")) {
		t.Errorf("subpage Value(0) = %q, want %q", sr.Value(0), "val1")
	}
}

func TestLeafNestedTreeEntry(t *testing.T) {
	cfg := PageConfig{PageSize: 4096, PageChecksum: false}
	buf := make([]byte, cfg.PageSize)

	b := NewLeafBuilder(buf, cfg)
	b.AddNestedTree([]byte("bigset"), 999, 50000)
	b.Finish()

	r := NewLeafReader(buf, cfg)
	keyBuf := make([]byte, 0, 64)
	e, _ := r.EntryAt(0, keyBuf)

	if !bytes.Equal(e.Key, []byte("bigset")) {
		t.Errorf("key = %q, want %q", e.Key, "bigset")
	}
	if e.CellFlags != CellFlagMultiValue|CellFlagNestedTree {
		t.Errorf("CellFlags = %d, want %d", e.CellFlags, CellFlagMultiValue|CellFlagNestedTree)
	}
	if e.NestedRoot != 999 {
		t.Errorf("NestedRoot = %d, want 999", e.NestedRoot)
	}
	if e.NestedCount != 50000 {
		t.Errorf("NestedCount = %d, want 50000", e.NestedCount)
	}
}

func TestLeafEntryAt(t *testing.T) {
	cfg := PageConfig{PageSize: 4096, PageChecksum: false}
	buf := make([]byte, cfg.PageSize)

	b := NewLeafBuilder(buf, cfg)
	for i := range 20 {
		key := fmt.Sprintf("key-%04d", i)
		val := fmt.Sprintf("val-%04d", i)
		b.AddInline([]byte(key), []byte(val))
	}
	b.Finish()

	r := NewLeafReader(buf, cfg)
	keyBuf := make([]byte, 0, 64)

	// Access entries out of order.
	for _, idx := range []int{15, 0, 19, 7, 16} {
		e, keyBuf2 := r.EntryAt(idx, keyBuf)
		keyBuf = keyBuf2
		wantKey := fmt.Sprintf("key-%04d", idx)
		wantVal := fmt.Sprintf("val-%04d", idx)
		if !bytes.Equal(e.Key, []byte(wantKey)) {
			t.Errorf("EntryAt(%d): key = %q, want %q", idx, e.Key, wantKey)
		}
		if !bytes.Equal(e.Value, []byte(wantVal)) {
			t.Errorf("EntryAt(%d): value = %q, want %q", idx, e.Value, wantVal)
		}
	}
}

func TestLeafEmpty(t *testing.T) {
	cfg := PageConfig{PageSize: 4096, PageChecksum: false}
	buf := make([]byte, cfg.PageSize)

	b := NewLeafBuilder(buf, cfg)
	b.Finish()

	r := NewLeafReader(buf, cfg)
	if r.Count() != 0 {
		t.Errorf("Count() = %d, want 0", r.Count())
	}

	keyBuf := make([]byte, 0, 64)
	idx, _, found := r.SearchLeaf([]byte("anything"), keyBuf)
	if found {
		t.Error("SearchLeaf on empty leaf: unexpectedly found")
	}
	if idx != 0 {
		t.Errorf("SearchLeaf on empty leaf: index = %d, want 0", idx)
	}

	// Iter on empty leaf should produce nothing.
	it := r.Iter(nil)
	if _, ok := it.Next(); ok {
		t.Error("Iter on empty leaf produced an entry")
	}
}

func TestLeafMixedCellTypes(t *testing.T) {
	cfg := PageConfig{PageSize: 4096, PageChecksum: false}
	buf := make([]byte, cfg.PageSize)

	// Build subpage data.
	spBuf := make([]byte, 64)
	spb := NewSubpageBuilder(spBuf, 0)
	spb.AddValue([]byte("a"))
	spSize := spb.Finish()

	b := NewLeafBuilder(buf, cfg)
	b.AddInline([]byte("key-0001"), []byte("inline-value"))
	b.AddOverflow([]byte("key-0002"), 100, 999999)
	b.AddSubpage([]byte("key-0003"), spBuf[:spSize])
	b.AddNestedTree([]byte("key-0004"), 200, 10000)
	b.AddInline([]byte("key-0005"), []byte("another-inline"))
	b.Finish()

	r := NewLeafReader(buf, cfg)
	if r.Count() != 5 {
		t.Fatalf("Count() = %d, want 5", r.Count())
	}

	keyBuf := make([]byte, 0, 128)

	// Check each entry type.
	e, keyBuf := r.EntryAt(0, keyBuf)
	if e.CellFlags != 0 || !bytes.Equal(e.Value, []byte("inline-value")) {
		t.Errorf("entry 0: unexpected flags=%d or value=%q", e.CellFlags, e.Value)
	}

	e, keyBuf = r.EntryAt(1, keyBuf)
	if e.CellFlags&CellFlagOverflow == 0 || e.OvflPage != 100 {
		t.Errorf("entry 1: overflow flag=%v, OvflPage=%d", e.CellFlags&CellFlagOverflow != 0, e.OvflPage)
	}

	e, keyBuf = r.EntryAt(2, keyBuf)
	if e.CellFlags&CellFlagMultiValue == 0 || e.CellFlags&CellFlagNestedTree != 0 {
		t.Errorf("entry 2: unexpected flags=%d", e.CellFlags)
	}

	e, keyBuf = r.EntryAt(3, keyBuf)
	if e.CellFlags != CellFlagMultiValue|CellFlagNestedTree {
		t.Errorf("entry 3: flags=%d, want %d", e.CellFlags, CellFlagMultiValue|CellFlagNestedTree)
	}

	e, _ = r.EntryAt(4, keyBuf)
	if e.CellFlags != 0 || !bytes.Equal(e.Value, []byte("another-inline")) {
		t.Errorf("entry 4: unexpected flags=%d or value=%q", e.CellFlags, e.Value)
	}
}

func TestLeafWithChecksum(t *testing.T) {
	cfg := PageConfig{PageSize: 4096, PageChecksum: true}
	buf := make([]byte, cfg.PageSize)

	b := NewLeafBuilder(buf, cfg)
	b.AddInline([]byte("key1"), []byte("val1"))
	b.AddInline([]byte("key2"), []byte("val2"))
	b.Finish()

	WriteCRC32C(buf)
	if !VerifyCRC32C(buf) {
		t.Fatal("checksum verification failed")
	}

	r := NewLeafReader(buf, cfg)
	if r.Count() != 2 {
		t.Fatalf("Count() = %d, want 2", r.Count())
	}
}

func TestSharedPrefixLen(t *testing.T) {
	tests := []struct {
		a, b string
		want int
	}{
		{"", "", 0},
		{"abc", "abc", 3},
		{"abc", "abd", 2},
		{"abc", "xyz", 0},
		{"abc", "ab", 2},
		{"ab", "abc", 2},
		{"", "abc", 0},
	}
	for _, tt := range tests {
		got := sharedPrefixLen([]byte(tt.a), []byte(tt.b))
		if got != tt.want {
			t.Errorf("sharedPrefixLen(%q, %q) = %d, want %d", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestLeafSingleEntry(t *testing.T) {
	cfg := PageConfig{PageSize: 4096, PageChecksum: false}
	buf := make([]byte, cfg.PageSize)

	b := NewLeafBuilder(buf, cfg)
	b.AddInline([]byte("only"), []byte("one"))
	b.Finish()

	r := NewLeafReader(buf, cfg)
	if r.Count() != 1 {
		t.Fatalf("Count() = %d, want 1", r.Count())
	}
	if r.RestartCount() != 1 {
		t.Errorf("RestartCount() = %d, want 1", r.RestartCount())
	}

	keyBuf := make([]byte, 0, 64)
	idx, e, found := r.SearchLeaf([]byte("only"), keyBuf)
	if !found || idx != 0 {
		t.Errorf("SearchLeaf(only): idx=%d found=%v", idx, found)
	}
	if !bytes.Equal(e.Value, []byte("one")) {
		t.Errorf("value = %q, want %q", e.Value, "one")
	}
}

func TestLeafExactrestartInterval(t *testing.T) {
	cfg := PageConfig{PageSize: 4096, PageChecksum: false}
	buf := make([]byte, cfg.PageSize)

	// Exactly 16 entries = 1 full restart group.
	b := NewLeafBuilder(buf, cfg)
	for i := range 16 {
		key := fmt.Sprintf("key-%04d", i)
		b.AddInline([]byte(key), []byte("v"))
	}
	b.Finish()

	r := NewLeafReader(buf, cfg)
	if r.Count() != 16 {
		t.Fatalf("Count() = %d, want 16", r.Count())
	}
	if r.RestartCount() != 1 {
		t.Errorf("RestartCount() = %d, want 1", r.RestartCount())
	}

	// Search for last entry (delta at position 15).
	keyBuf := make([]byte, 0, 64)
	_, _, found := r.SearchLeaf([]byte("key-0015"), keyBuf)
	if !found {
		t.Error("SearchLeaf(key-0015): not found")
	}
}

func TestLeafSecondGroupBoundary(t *testing.T) {
	cfg := PageConfig{PageSize: 4096, PageChecksum: false}
	buf := make([]byte, cfg.PageSize)

	// 17 entries = first group (16) + second group (1 restart).
	b := NewLeafBuilder(buf, cfg)
	for i := range 17 {
		key := fmt.Sprintf("key-%04d", i)
		b.AddInline([]byte(key), []byte("v"))
	}
	b.Finish()

	r := NewLeafReader(buf, cfg)
	if r.Count() != 17 {
		t.Fatalf("Count() = %d, want 17", r.Count())
	}
	if r.RestartCount() != 2 {
		t.Errorf("RestartCount() = %d, want 2", r.RestartCount())
	}

	// Search for entry 16 (first entry of second group, a restart).
	keyBuf := make([]byte, 0, 64)
	idx, _, found := r.SearchLeaf([]byte("key-0016"), keyBuf)
	if !found {
		t.Error("SearchLeaf(key-0016): not found")
	}
	if idx != 16 {
		t.Errorf("index = %d, want 16", idx)
	}

	// EntryAt for entry 16 should also work.
	e, _ := r.EntryAt(16, keyBuf)
	if !bytes.Equal(e.Key, []byte("key-0016")) {
		t.Errorf("EntryAt(16): key = %q, want %q", e.Key, "key-0016")
	}
}

func TestLeafIter(t *testing.T) {
	cfg := PageConfig{PageSize: 4096, PageChecksum: false}
	buf := make([]byte, cfg.PageSize)

	// 35 entries = group 0 (16), group 1 (16), group 2 (3).
	b := NewLeafBuilder(buf, cfg)
	for i := range 35 {
		key := fmt.Sprintf("key-%04d", i)
		val := fmt.Sprintf("val-%04d", i)
		b.AddInline([]byte(key), []byte(val))
	}
	b.Finish()

	r := NewLeafReader(buf, cfg)

	// Iter over all entries.
	idx := 0
	it := r.Iter(nil)
	for e, ok := it.Next(); ok; e, ok = it.Next() {
		wantKey := fmt.Sprintf("key-%04d", idx)
		wantVal := fmt.Sprintf("val-%04d", idx)
		if !bytes.Equal(e.Key, []byte(wantKey)) {
			t.Errorf("entry %d: key = %q, want %q", idx, e.Key, wantKey)
		}
		if !bytes.Equal(e.Value, []byte(wantVal)) {
			t.Errorf("entry %d: value = %q, want %q", idx, e.Value, wantVal)
		}
		idx++
	}
	if idx != 35 {
		t.Errorf("iterated %d entries, want 35", idx)
	}
}

func TestLeafGroupIter(t *testing.T) {
	cfg := PageConfig{PageSize: 4096, PageChecksum: false}
	buf := make([]byte, cfg.PageSize)

	// 35 entries = group 0 (16), group 1 (16), group 2 (3).
	b := NewLeafBuilder(buf, cfg)
	for i := range 35 {
		key := fmt.Sprintf("key-%04d", i)
		val := fmt.Sprintf("val-%04d", i)
		b.AddInline([]byte(key), []byte(val))
	}
	b.Finish()

	r := NewLeafReader(buf, cfg)

	groups := []struct {
		groupIdx int
		startIdx int
		count    int
	}{
		{0, 0, 16},
		{1, 16, 16},
		{2, 32, 3},
	}
	for _, g := range groups {
		n := 0
		it := r.GroupIter(g.groupIdx, nil)
		for e, ok := it.Next(); ok; e, ok = it.Next() {
			wantKey := fmt.Sprintf("key-%04d", g.startIdx+n)
			if !bytes.Equal(e.Key, []byte(wantKey)) {
				t.Errorf("group %d entry %d: key = %q, want %q", g.groupIdx, n, e.Key, wantKey)
			}
			n++
		}
		if n != g.count {
			t.Errorf("group %d: iterated %d entries, want %d", g.groupIdx, n, g.count)
		}
	}
}

func TestLeafGroupIterSingleEntry(t *testing.T) {
	cfg := PageConfig{PageSize: 4096, PageChecksum: false}
	buf := make([]byte, cfg.PageSize)

	b := NewLeafBuilder(buf, cfg)
	b.AddInline([]byte("only-key"), []byte("only-val"))
	b.Finish()

	r := NewLeafReader(buf, cfg)
	it := r.GroupIter(0, nil)
	e, ok := it.Next()
	if !ok {
		t.Fatal("GroupIter produced no entries")
	}
	if !bytes.Equal(e.Key, []byte("only-key")) {
		t.Errorf("key = %q, want %q", e.Key, "only-key")
	}
	if !bytes.Equal(e.Value, []byte("only-val")) {
		t.Errorf("value = %q, want %q", e.Value, "only-val")
	}
	if _, ok := it.Next(); ok {
		t.Error("GroupIter produced extra entry")
	}
}

func TestLeafIterKeyBufReuse(t *testing.T) {
	cfg := PageConfig{PageSize: 4096, PageChecksum: false}
	buf := make([]byte, cfg.PageSize)

	b := NewLeafBuilder(buf, cfg)
	for i := range 20 {
		b.AddInline([]byte(fmt.Sprintf("key-%04d", i)), []byte("v"))
	}
	b.Finish()

	r := NewLeafReader(buf, cfg)

	// First iteration seeds the keyBuf.
	it1 := r.GroupIter(0, nil)
	for _, ok := it1.Next(); ok; _, ok = it1.Next() {
	}

	// Second iteration reuses the keyBuf — should not allocate.
	it2 := r.GroupIter(1, it1.KeyBuf())
	n := 0
	for e, ok := it2.Next(); ok; e, ok = it2.Next() {
		wantKey := fmt.Sprintf("key-%04d", 16+n)
		if !bytes.Equal(e.Key, []byte(wantKey)) {
			t.Errorf("entry %d: key = %q, want %q", n, e.Key, wantKey)
		}
		n++
	}
	if n != 4 {
		t.Errorf("iterated %d entries, want 4", n)
	}
}

func TestLeafBuilderFreeSpaceAndCount(t *testing.T) {
	cfg := PageConfig{PageSize: 4096, PageChecksum: false}
	buf := make([]byte, cfg.PageSize)

	b := NewLeafBuilder(buf, cfg)
	if b.Count() != 0 {
		t.Errorf("Count() = %d, want 0", b.Count())
	}

	initialFree := b.FreeSpace()
	if initialFree <= 0 {
		t.Fatalf("FreeSpace() = %d, want > 0", initialFree)
	}

	b.AddInline([]byte("key"), []byte("val"))
	if b.Count() != 1 {
		t.Errorf("Count() = %d, want 1", b.Count())
	}
	if b.FreeSpace() >= initialFree {
		t.Error("FreeSpace did not decrease after AddInline")
	}
}

func TestLeafBuilderFull(t *testing.T) {
	cfg := PageConfig{PageSize: 4096, PageChecksum: false}
	buf := make([]byte, cfg.PageSize)

	b := NewLeafBuilder(buf, cfg)
	count := 0
	for {
		key := fmt.Sprintf("k%06d", count)
		val := fmt.Sprintf("v%06d", count)
		if !b.AddInline([]byte(key), []byte(val)) {
			break
		}
		count++
	}
	b.Finish()

	if count == 0 {
		t.Fatal("expected at least one entry to fit")
	}

	// Verify all entries round-trip.
	r := NewLeafReader(buf, cfg)
	if r.Count() != count {
		t.Fatalf("Count() = %d, want %d", r.Count(), count)
	}

	idx := 0
	it := r.Iter(nil)
	for e, ok := it.Next(); ok; e, ok = it.Next() {
		wantKey := fmt.Sprintf("k%06d", idx)
		if !bytes.Equal(e.Key, []byte(wantKey)) {
			t.Errorf("entry %d: key = %q, want %q", idx, e.Key, wantKey)
		}
		idx++
	}
}

func FuzzLeafRoundTrip(f *testing.F) {
	f.Add([]byte("key1"), []byte("val1"), []byte("key2"), []byte("val2"))
	f.Add([]byte("aaaa"), []byte(""), []byte("aaab"), []byte("x"))
	f.Add([]byte{0x00}, []byte{0xFF}, []byte{0x01}, []byte{0x00})

	f.Fuzz(func(t *testing.T, k1, v1, k2, v2 []byte) {
		if len(k1) == 0 || len(k2) == 0 {
			return
		}
		// Ensure sorted order.
		if bytes.Compare(k1, k2) >= 0 {
			return
		}

		cfg := PageConfig{PageSize: 4096, PageChecksum: false}
		buf := make([]byte, cfg.PageSize)
		b := NewLeafBuilder(buf, cfg)

		if !b.AddInline(k1, v1) {
			return
		}
		if !b.AddInline(k2, v2) {
			return
		}
		b.Finish()

		r := NewLeafReader(buf, cfg)
		if r.Count() != 2 {
			t.Fatalf("Count() = %d, want 2", r.Count())
		}

		keyBuf := make([]byte, 0, 256)

		// Verify first entry.
		e, keyBuf := r.EntryAt(0, keyBuf)
		if !bytes.Equal(e.Key, k1) {
			t.Errorf("entry 0: key mismatch")
		}
		if !bytes.Equal(e.Value, v1) {
			t.Errorf("entry 0: value mismatch")
		}

		// Verify second entry.
		e, _ = r.EntryAt(1, keyBuf)
		if !bytes.Equal(e.Key, k2) {
			t.Errorf("entry 1: key mismatch")
		}
		if !bytes.Equal(e.Value, v2) {
			t.Errorf("entry 1: value mismatch")
		}

		// Verify search finds both.
		keyBuf = keyBuf[:0]
		_, _, found := r.SearchLeaf(k1, keyBuf)
		if !found {
			t.Error("SearchLeaf(k1): not found")
		}
		_, _, found = r.SearchLeaf(k2, keyBuf)
		if !found {
			t.Error("SearchLeaf(k2): not found")
		}
	})
}

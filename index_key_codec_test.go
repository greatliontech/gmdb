package gmdb

import (
	"bytes"
	"errors"
	"math/rand/v2"
	"sort"
	"strings"
	"testing"
)

// --- escape/unescape unit tests -----------------------------------

// TestEscapeColumnPassthrough verifies that a column with no 0x00
// bytes passes through unchanged (but freshly allocated).
func TestEscapeColumnPassthrough(t *testing.T) {
	in := []byte("hello world")
	out := escapeColumn(in)
	if !bytes.Equal(out, in) {
		t.Fatalf("passthrough mismatch: got %x want %x", out, in)
	}
	// Confirm fresh allocation: mutate out, in should be unchanged.
	out[0] = 'X'
	if in[0] != 'h' {
		t.Errorf("escapeColumn aliased input slice")
	}
}

// TestEscapeColumnEscapesZeros verifies every 0x00 → 0x00 0xFF.
func TestEscapeColumnEscapesZeros(t *testing.T) {
	in := []byte{0x00, 0x41, 0x00, 0x42, 0x00}
	want := []byte{0x00, 0xFF, 0x41, 0x00, 0xFF, 0x42, 0x00, 0xFF}
	got := escapeColumn(in)
	if !bytes.Equal(got, want) {
		t.Errorf("escape: got %x want %x", got, want)
	}
}

// TestEscapeColumnEmpty verifies an empty input encodes to empty.
func TestEscapeColumnEmpty(t *testing.T) {
	got := escapeColumn(nil)
	if len(got) != 0 {
		t.Errorf("empty input → non-empty output %x", got)
	}
	got = escapeColumn([]byte{})
	if len(got) != 0 {
		t.Errorf("empty slice → non-empty output %x", got)
	}
}

// TestUnescapeColumnRoundtrip verifies escape → unescape → original
// across a range of inputs including all-zeros, mixed, and high-byte
// values (0xFF passes through escape unchanged).
func TestUnescapeColumnRoundtrip(t *testing.T) {
	cases := [][]byte{
		nil,
		{},
		{0x41, 0x42, 0x43},
		{0x00},
		{0x00, 0x00, 0x00},
		{0xFF, 0xFF, 0xFF},
		{0x00, 0xFF, 0x00, 0xFF},
		{0x00, 0x01, 0xFE, 0xFF}, // includes the lex-distinct 0x01
	}
	for i, in := range cases {
		esc := escapeColumn(in)
		got, err := unescapeColumn(esc)
		if err != nil {
			t.Errorf("case %d: unescape %x: %v", i, esc, err)
			continue
		}
		if !bytes.Equal(got, in) {
			// nil vs empty slice equality oddity — normalize.
			if len(got) == 0 && len(in) == 0 {
				continue
			}
			t.Errorf("case %d: roundtrip lost data: in=%x esc=%x out=%x", i, in, esc, got)
		}
	}
}

// TestUnescapeColumnRejectsLone00 verifies that a 0x00 not followed
// by 0xFF in the (already-extracted) escaped column body returns
// errIndexKeyMalformed.
func TestUnescapeColumnRejectsLone00(t *testing.T) {
	// 0x00 followed by 0x41 — invalid in column body.
	_, err := unescapeColumn([]byte{0x00, 0x41})
	if !errors.Is(err, errIndexKeyMalformed) {
		t.Errorf("expected errIndexKeyMalformed, got %v", err)
	}
}

// TestUnescapeColumnRejectsTrailing00 verifies that a 0x00 as the
// last byte (no following byte) is malformed.
func TestUnescapeColumnRejectsTrailing00(t *testing.T) {
	_, err := unescapeColumn([]byte{0x41, 0x00})
	if !errors.Is(err, errIndexKeyMalformed) {
		t.Errorf("expected errIndexKeyMalformed, got %v", err)
	}
}

// --- encode/decode unit tests -------------------------------------

// TestEncodeIndexKeySingleColumn verifies the simplest case: one
// column → escaped + 0x00 0x00 terminator.
func TestEncodeIndexKeySingleColumn(t *testing.T) {
	got := encodeIndexKey([][]byte{[]byte("owner")})
	want := []byte{0x6F, 0x77, 0x6E, 0x65, 0x72, 0x00, 0x00}
	if !bytes.Equal(got, want) {
		t.Errorf("got %x want %x", got, want)
	}
}

// TestEncodeIndexKeyEmptyColumn verifies an empty column encodes to
// a bare 0x00 0x00 terminator. Per the spec worked example T1 (Col
// A empty, Col B [0x00]): encoded `00 00  00 FF 00 00`.
func TestEncodeIndexKeyEmptyColumn(t *testing.T) {
	got := encodeIndexKey([][]byte{{}, {0x00}})
	want := []byte{0x00, 0x00, 0x00, 0xFF, 0x00, 0x00}
	if !bytes.Equal(got, want) {
		t.Errorf("got %x want %x", got, want)
	}
}

// TestEncodeIndexKeySpecWorkedExamples verifies the three tuples
// from page-formats.md §NUL-escape encoding §Worked example
// encode to the spec-cited byte sequences and that they sort
// lex-correctly (T1 < T2 < T3).
func TestEncodeIndexKeySpecWorkedExamples(t *testing.T) {
	t1 := encodeIndexKey([][]byte{{}, {0x00}})
	t2 := encodeIndexKey([][]byte{{0x00}, {}})
	t3 := encodeIndexKey([][]byte{{0x00, 0xFF}, {0x00}})

	wantT1 := []byte{0x00, 0x00, 0x00, 0xFF, 0x00, 0x00}
	wantT2 := []byte{0x00, 0xFF, 0x00, 0x00, 0x00, 0x00}
	wantT3 := []byte{0x00, 0xFF, 0xFF, 0x00, 0x00, 0x00, 0xFF, 0x00, 0x00}

	if !bytes.Equal(t1, wantT1) {
		t.Errorf("T1: got %x want %x", t1, wantT1)
	}
	if !bytes.Equal(t2, wantT2) {
		t.Errorf("T2: got %x want %x", t2, wantT2)
	}
	if !bytes.Equal(t3, wantT3) {
		t.Errorf("T3: got %x want %x", t3, wantT3)
	}
	// Spec claim: byte-wise comparison yields T1 < T2 < T3.
	if bytes.Compare(t1, t2) >= 0 {
		t.Errorf("T1 >= T2 byte-wise: %x vs %x", t1, t2)
	}
	if bytes.Compare(t2, t3) >= 0 {
		t.Errorf("T2 >= T3 byte-wise: %x vs %x", t2, t3)
	}
}

// TestDecodeIndexKeyRoundtrip verifies that a representative range
// of column tuples encode + decode back to identity.
func TestDecodeIndexKeyRoundtrip(t *testing.T) {
	cases := [][][]byte{
		{[]byte("owner")},
		{[]byte("owner"), []byte("repo")},
		{{}, {0x00}, {0xFF}},
		{{0x00, 0x01, 0x02}, {0xFE, 0xFF}},
		{[]byte("multi"), []byte("column"), []byte("tuple")},
	}
	for i, cols := range cases {
		enc := encodeIndexKey(cols)
		dec, err := decodeIndexKey(enc)
		if err != nil {
			t.Errorf("case %d: decode %x: %v", i, enc, err)
			continue
		}
		if len(dec) != len(cols) {
			t.Errorf("case %d: column count mismatch: got %d want %d", i, len(dec), len(cols))
			continue
		}
		for j := range cols {
			if !bytes.Equal(dec[j], cols[j]) {
				if len(dec[j]) == 0 && len(cols[j]) == 0 {
					continue
				}
				t.Errorf("case %d col %d: got %x want %x", i, j, dec[j], cols[j])
			}
		}
	}
}

// TestDecodeIndexKeyRejectsUnterminatedKey verifies that a key
// without a trailing 0x00 0x00 is malformed.
func TestDecodeIndexKeyRejectsUnterminatedKey(t *testing.T) {
	// "owner" with no terminator — last column not terminated.
	_, err := decodeIndexKey([]byte{0x6F, 0x77, 0x6E, 0x65, 0x72})
	if !errors.Is(err, errIndexKeyMalformed) {
		t.Errorf("expected malformed, got %v", err)
	}
}

// TestDecodeIndexKeyRejects0x00x01Separator verifies that the
// SetKeyspace separator 0x00 0x01 (lex-distinct from 0x00 0x00 and
// 0x00 0xFF) is rejected by the strict decoder — confirms the codec
// is purely the chunk-7.4 NUL-escape grammar and not lenient.
// SetKeyspace compound-PK decoding at chunk 7.9 handles 0x00 0x01
// ad-hoc.
func TestDecodeIndexKeyRejects0x00x01Separator(t *testing.T) {
	// A literal 0x00 0x01 mid-column — must reject.
	_, err := decodeIndexKey([]byte{0x6F, 0x00, 0x01, 0x77, 0x00, 0x00})
	if !errors.Is(err, errIndexKeyMalformed) {
		t.Errorf("expected malformed on 0x00 0x01 sequence, got %v", err)
	}
}

// TestDecodeIndexKeyEmpty verifies decoding the empty key returns
// no columns.
func TestDecodeIndexKeyEmpty(t *testing.T) {
	got, err := decodeIndexKey(nil)
	if err != nil {
		t.Errorf("nil: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("nil: got %v want empty", got)
	}
	got, err = decodeIndexKey([]byte{})
	if err != nil {
		t.Errorf("empty: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("empty: got %v want empty", got)
	}
}

// --- Prefix-freeness invariant tests ------------------------------

// TestEncodedColumnPrefixFreeness verifies the clause-explicit
// invariant from page-formats.md §Invariants: no escaped column is
// a prefix of another's encoded form (including terminator). Equiv:
// after appending the 0x00 0x00 terminator, two distinct column
// values produce two encoded forms where neither is a prefix of
// the other.
func TestEncodedColumnPrefixFreeness(t *testing.T) {
	// Deterministic edge cases that historically have broken
	// naive encodings (e.g. without the terminator-prefix-freeness
	// guarantee):
	pairs := [][2][]byte{
		{{}, {0x00}},           // empty vs single-zero
		{{0x00}, {0x00, 0x00}}, // single-zero vs double-zero
		{{0x00, 0x00}, {0x00}}, // reverse
		{{0xFF}, {0xFF, 0xFF}}, // single-FF vs double-FF
		{{0x00, 0xFF}, {0x00}}, // escape pair vs single-zero
		{[]byte("a"), []byte("ab")},
		{[]byte("ab"), []byte("a")},
		{[]byte("a"), []byte("a\x00")},
		{[]byte("a\x00"), []byte("a")},
	}
	for i, p := range pairs {
		if bytes.Equal(p[0], p[1]) {
			t.Fatalf("pair %d: test-data error — equal byte slices %x", i, p[0])
		}
		encA := encodeIndexKey([][]byte{p[0]})
		encB := encodeIndexKey([][]byte{p[1]})
		if bytes.HasPrefix(encA, encB) {
			t.Errorf("pair %d: enc(%x) has prefix enc(%x); raw=%x prefix=%x",
				i, p[0], p[1], encA, encB)
		}
		if bytes.HasPrefix(encB, encA) {
			t.Errorf("pair %d: enc(%x) has prefix enc(%x); raw=%x prefix=%x",
				i, p[1], p[0], encB, encA)
		}
	}
}

// TestEncodedTuplePrefixFreenessSameColumnCount verifies the
// stronger prefix-freeness property: among distinct tuples
// SHARING THE SAME COLUMN COUNT, no encoded form is a prefix of
// another's. This is the operational invariant that index range
// queries depend on — an index is keyed by a fixed-schema column
// tuple, and at the same column count the encoder must produce
// mutually-non-prefix bytes so lex-order on the encoded bytes
// matches the tuple-lex order without confusion.
//
// Tuples of DIFFERENT column counts CAN have one's encoding be a
// prefix of another's (e.g. enc([a,b]) is a prefix of enc([a,b,c]))
// — that is by design and not in scope for the invariant (a single
// index has a fixed schema, so the decoder always processes the
// same column count).
func TestEncodedTuplePrefixFreenessSameColumnCount(t *testing.T) {
	groups := map[int][][][]byte{
		1: {
			{[]byte("a")},
			{[]byte("ab")},
			{[]byte("b")},
			{{0x00, 0xFF}},
			{{0x00}},
			{{}},
		},
		2: {
			{[]byte("a"), []byte("b")},
			{[]byte("a"), []byte("bc")},
			{[]byte("ab"), []byte("c")},
			{[]byte("a"), []byte("b\x00")},
			{[]byte("a\x00"), []byte("b")},
			{{}, {}},
			{{}, {0x00}},
		},
		3: {
			{[]byte("a"), []byte("b"), []byte("c")},
			{[]byte("a"), []byte("b"), []byte("cd")},
			{{}, {}, {}},
			{[]byte("a"), {}, []byte("c")},
		},
	}
	for nCols, tuples := range groups {
		encs := make([][]byte, len(tuples))
		for i, tup := range tuples {
			encs[i] = encodeIndexKey(tup)
		}
		for i := range encs {
			for j := range encs {
				if i == j {
					continue
				}
				if bytes.HasPrefix(encs[i], encs[j]) && !bytes.Equal(encs[i], encs[j]) {
					t.Errorf("nCols=%d: tuple %d enc=%x has prefix tuple %d enc=%x (tuples %v vs %v)",
						nCols, i, encs[i], j, encs[j],
						byteSliceSliceString(tuples[i]), byteSliceSliceString(tuples[j]))
				}
			}
		}
	}
}

// TestEncodedColumnPrefixFreenessProperty fuzz-tests prefix-
// freeness across many random column-pair inputs. The property
// is: for distinct columns A != B, neither encodeIndexKey([A]) nor
// encodeIndexKey([B]) is a prefix of the other.
func TestEncodedColumnPrefixFreenessProperty(t *testing.T) {
	r := rand.New(rand.NewPCG(0xDEAD, 0xBEEF))
	const iterations = 2000
	for it := range iterations {
		a := randBytes(r, 0, 32)
		b := randBytes(r, 0, 32)
		if bytes.Equal(a, b) {
			continue
		}
		encA := encodeIndexKey([][]byte{a})
		encB := encodeIndexKey([][]byte{b})
		if bytes.HasPrefix(encA, encB) {
			t.Fatalf("iter %d: enc(%x) has prefix enc(%x); encA=%x encB=%x",
				it, a, b, encA, encB)
		}
		if bytes.HasPrefix(encB, encA) {
			t.Fatalf("iter %d: enc(%x) has prefix enc(%x); encA=%x encB=%x",
				it, b, a, encA, encB)
		}
	}
}

// TestEncodedTuplePrefixFreenessSameNColsProperty fuzz-tests the
// stronger property at the tuple level: for two distinct tuples
// of the SAME column count, neither encoded form is a prefix of
// the other. (Different column counts can prefix-collide by
// design.)
func TestEncodedTuplePrefixFreenessSameNColsProperty(t *testing.T) {
	r := rand.New(rand.NewPCG(0xBAD0, 0xC0DE))
	const iterations = 2000
	for it := range iterations {
		nCols := 1 + r.IntN(4)
		a := make([][]byte, nCols)
		b := make([][]byte, nCols)
		for i := range nCols {
			a[i] = randBytes(r, 0, 16)
			b[i] = randBytes(r, 0, 16)
		}
		// Skip equal tuples (no prefix claim to make).
		eq := true
		for i := range nCols {
			if !bytes.Equal(a[i], b[i]) {
				eq = false
				break
			}
		}
		if eq {
			continue
		}
		encA := encodeIndexKey(a)
		encB := encodeIndexKey(b)
		if bytes.HasPrefix(encA, encB) {
			t.Fatalf("iter %d nCols=%d: enc(%v) has prefix enc(%v); encA=%x encB=%x",
				it, nCols, byteSliceSliceString(a), byteSliceSliceString(b), encA, encB)
		}
		if bytes.HasPrefix(encB, encA) {
			t.Fatalf("iter %d nCols=%d: enc(%v) has prefix enc(%v); encA=%x encB=%x",
				it, nCols, byteSliceSliceString(b), byteSliceSliceString(a), encB, encA)
		}
	}
}

// TestEncodedKeyLexOrderingPreserved verifies the spec's lex-order
// guarantee: byte-wise comparison of encoded keys matches the
// component-wise lex order of the original tuples. Random property
// test over column tuples.
func TestEncodedKeyLexOrderingPreserved(t *testing.T) {
	r := rand.New(rand.NewPCG(0xCAFE, 0xF00D))
	const iterations = 2000
	for it := range iterations {
		nCols := 1 + r.IntN(4)
		tupA := make([][]byte, nCols)
		tupB := make([][]byte, nCols)
		for i := range nCols {
			tupA[i] = randBytes(r, 0, 16)
			tupB[i] = randBytes(r, 0, 16)
		}
		encA := encodeIndexKey(tupA)
		encB := encodeIndexKey(tupB)
		lexA := lexCompareTuples(tupA, tupB)
		lexEnc := bytes.Compare(encA, encB)
		if sign(lexA) != sign(lexEnc) {
			t.Fatalf("iter %d: lex order mismatch — tuples %v vs %v lex=%d, enc lex=%d (encA=%x encB=%x)",
				it, byteSliceSliceString(tupA), byteSliceSliceString(tupB), lexA, lexEnc, encA, encB)
		}
	}
}

// TestEncodedKeySortsLikeTuples is a deterministic counterpart to
// the property test: an explicit list of tuples sorted by tuple-
// lex order should also sort by encoded byte-lex order.
func TestEncodedKeySortsLikeTuples(t *testing.T) {
	tuples := [][][]byte{
		{[]byte("a")},
		{[]byte("ab")},
		{[]byte("b")},
		{{}, {}},
		{{}, {0x00}},
		{{0x00}, {}},
		{{0x00, 0xFF}, {0x00}},
	}
	// Make a copy sorted by tuple-lex.
	byTuple := make([][][]byte, len(tuples))
	copy(byTuple, tuples)
	sort.Slice(byTuple, func(i, j int) bool {
		return lexCompareTuples(byTuple[i], byTuple[j]) < 0
	})
	// Encode each, sort by encoded-byte-lex.
	type pair struct {
		tup [][]byte
		enc []byte
	}
	pairs := make([]pair, len(tuples))
	for i, t := range tuples {
		pairs[i] = pair{tup: t, enc: encodeIndexKey(t)}
	}
	sort.Slice(pairs, func(i, j int) bool {
		return bytes.Compare(pairs[i].enc, pairs[j].enc) < 0
	})
	// Compare the two orderings.
	for i := range tuples {
		if lexCompareTuples(byTuple[i], pairs[i].tup) != 0 {
			t.Errorf("ordering mismatch at index %d: byTuple=%v byEnc=%v",
				i, byteSliceSliceString(byTuple[i]), byteSliceSliceString(pairs[i].tup))
		}
	}
}

// --- helpers ------------------------------------------------------

func randBytes(r *rand.Rand, minLen, maxLen int) []byte {
	n := minLen
	if maxLen > minLen {
		n = minLen + r.IntN(maxLen-minLen+1)
	}
	if n == 0 {
		return nil
	}
	out := make([]byte, n)
	for i := range out {
		out[i] = byte(r.UintN(256))
	}
	return out
}

func lexCompareTuples(a, b [][]byte) int {
	n := min(len(a), len(b))
	for i := range n {
		c := bytes.Compare(a[i], b[i])
		if c != 0 {
			return c
		}
	}
	switch {
	case len(a) < len(b):
		return -1
	case len(a) > len(b):
		return 1
	}
	return 0
}

func sign(x int) int {
	switch {
	case x < 0:
		return -1
	case x > 0:
		return 1
	}
	return 0
}

func byteSliceSliceString(t [][]byte) string {
	parts := make([]string, len(t))
	for i, b := range t {
		parts[i] = string(b)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

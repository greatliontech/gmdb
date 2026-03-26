package page

// KsDesc is a decoded keyspace descriptor (32 bytes on disk).
//
// Byte layout:
//
//	 0..7:   Root           uint64
//	 8..15:  Count          uint64
//	16:      Kind           uint8
//	17..18:  FixedValueSize uint16
//	19..26:  NextSeq        uint64
//	27..31:  Reserved       [5]byte (must be zero)
type KsDesc struct {
	Root           uint64
	Count          uint64
	Kind           uint8
	FixedValueSize uint16
	NextSeq        uint64
}

// Keyspace descriptor field offsets.
const (
	ksOffRoot           = 0
	ksOffCount          = 8
	ksOffKind           = 16
	ksOffFixedValueSize = 17
	ksOffNextSeq        = 19
	ksOffReserved       = 27
)

// DecodeKsDesc decodes a 32-byte keyspace descriptor from buf.
func DecodeKsDesc(buf []byte) KsDesc {
	_ = buf[KsDescSize-1] // bounds check
	return KsDesc{
		Root:           le.Uint64(buf[ksOffRoot:]),
		Count:          le.Uint64(buf[ksOffCount:]),
		Kind:           buf[ksOffKind],
		FixedValueSize: le.Uint16(buf[ksOffFixedValueSize:]),
		NextSeq:        le.Uint64(buf[ksOffNextSeq:]),
	}
}

// EncodeKsDesc encodes a KsDesc into buf (must be >= 32 bytes).
// The 5 reserved bytes are zeroed.
func EncodeKsDesc(buf []byte, d *KsDesc) {
	_ = buf[KsDescSize-1] // bounds check
	le.PutUint64(buf[ksOffRoot:], d.Root)
	le.PutUint64(buf[ksOffCount:], d.Count)
	buf[ksOffKind] = d.Kind
	le.PutUint16(buf[ksOffFixedValueSize:], d.FixedValueSize)
	le.PutUint64(buf[ksOffNextSeq:], d.NextSeq)
	// Zero reserved bytes.
	clear(buf[ksOffReserved:KsDescSize])
}

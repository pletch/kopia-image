package ntfsfs

import (
	"encoding/binary"
	"slices"

	"github.com/pkg/errors"
)

// Offsets in an MFT record header and attribute headers.
const (
	recordHeaderMinSize   = 0x30
	usnSize               = 2
	bitsPerNibble         = 4
	lowNibble             = 0x0F
	residentHeaderSize    = 0x18
	nonResidentHeaderSize = 0x40
)

// mftRecord is a parsed, fixed-up MFT record.
type mftRecord struct {
	seq        uint16
	flags      uint16
	baseRef    uint64
	attributes []attribute
}

// attribute is an attribute header and its value or run list. The slices
// point into the record buffer.
type attribute struct {
	typ         uint32
	named       bool
	nonResident bool

	value []byte // resident

	firstVCN, lastVCN int64 // non-resident
	dataSize          int64
	runList           []byte
}

// parseRecord applies the update sequence fixups to an MFT record in place
// and parses its header and attributes, strictly: any inconsistency is an
// error.
func parseRecord(buf []byte, n int64) (*mftRecord, error) {
	switch string(buf[0:4]) {
	case "FILE":
	case "BAAD":
		return nil, errors.New("marked bad by a failed multi-sector write")
	default:
		return nil, errors.New("no FILE signature")
	}

	if err := applyFixups(buf); err != nil {
		return nil, err
	}

	le := binary.LittleEndian
	attrOff := int(le.Uint16(buf[0x14:]))
	used := int(le.Uint32(buf[0x18:]))

	if alloc := int(le.Uint32(buf[0x1C:])); alloc != len(buf) {
		return nil, errors.Errorf("allocated size %v, records are %v bytes", alloc, len(buf))
	}

	if used > len(buf) || attrOff < recordHeaderMinSize-8 || attrOff >= used || attrOff%8 != 0 {
		return nil, errors.Errorf("invalid header: attributes at %v, %v bytes used", attrOff, used)
	}

	// NTFS 3.1 records hold their own number
	if le.Uint16(buf[4:]) >= recordHeaderMinSize {
		if got := le.Uint32(buf[0x2C:]); int64(got) != n&0xFFFFFFFF {
			return nil, errors.Errorf("holds record number %v", got)
		}
	}

	rec := &mftRecord{
		seq:     le.Uint16(buf[0x10:]),
		flags:   le.Uint16(buf[0x16:]),
		baseRef: le.Uint64(buf[0x20:]),
	}

	attrs, err := parseAttributes(buf[:used], attrOff)
	if err != nil {
		return nil, err
	}

	rec.attributes = attrs

	return rec, nil
}

// applyFixups checks that every 512-byte stride of the record ends with the
// update sequence number, which proves the record was written whole, and
// restores the original bytes.
func applyFixups(buf []byte) error {
	le := binary.LittleEndian
	usaOff := int(le.Uint16(buf[4:]))
	usaCount := int(le.Uint16(buf[6:]))

	if usaCount != len(buf)/fixupStride+1 || usaOff < 0x28 || usaOff+2*usaCount > len(buf) {
		return errors.Errorf("invalid update sequence array (offset %v, %v entries)", usaOff, usaCount)
	}

	usn := buf[usaOff : usaOff+2]

	for i := 1; i < usaCount; i++ {
		end := i*fixupStride - usnSize

		if buf[end] != usn[0] || buf[end+1] != usn[1] {
			return errors.Errorf("torn write: sector %v of the record is from another write", i-1)
		}

		copy(buf[end:end+usnSize], buf[usaOff+usnSize*i:])
	}

	return nil
}

func parseAttributes(buf []byte, off int) ([]attribute, error) {
	le := binary.LittleEndian

	var attrs []attribute

	for {
		if off+4 > len(buf) {
			return nil, errors.New("the attribute list has no end marker")
		}

		typ := le.Uint32(buf[off:])
		if typ == attrTypeEnd {
			return attrs, nil
		}

		if off+16 > len(buf) {
			return nil, errors.Errorf("attribute at offset %v overruns the record", off)
		}

		length := int(le.Uint32(buf[off+4:]))
		if length < 16 || length%8 != 0 || off+length > len(buf) {
			return nil, errors.Errorf("attribute 0x%X at offset %v has invalid length %v", typ, off, length)
		}

		b := buf[off : off+length]

		a, err := parseAttribute(typ, b)
		if err != nil {
			return nil, errors.Wrapf(err, "attribute 0x%X at offset %v", typ, off)
		}

		attrs = append(attrs, a)
		off += length
	}
}

func parseAttribute(typ uint32, b []byte) (attribute, error) {
	le := binary.LittleEndian
	a := attribute{typ: typ, nonResident: b[8] != 0}

	if nameLen, nameOff := int(b[9]), int(le.Uint16(b[0x0A:])); nameLen > 0 {
		if nameOff+2*nameLen > len(b) {
			return a, errors.New("name overruns the attribute")
		}

		a.named = true
	}

	if !a.nonResident {
		if len(b) < residentHeaderSize {
			return a, errors.New("resident header too short")
		}

		valLen, valOff := int(le.Uint32(b[0x10:])), int(le.Uint16(b[0x14:]))
		if valOff+valLen > len(b) {
			return a, errors.New("value overruns the attribute")
		}

		a.value = b[valOff : valOff+valLen]

		return a, nil
	}

	if len(b) < nonResidentHeaderSize {
		return a, errors.New("non-resident header too short")
	}

	runOff := int(le.Uint16(b[0x20:]))
	if runOff < nonResidentHeaderSize || runOff >= len(b) {
		return a, errors.Errorf("run list at invalid offset %v", runOff)
	}

	a.firstVCN = int64(le.Uint64(b[0x10:])) //nolint:gosec
	a.lastVCN = int64(le.Uint64(b[0x18:]))  //nolint:gosec
	a.dataSize = int64(le.Uint64(b[0x30:])) //nolint:gosec
	a.runList = b[runOff:]

	return a, nil
}

// findAttribute returns the record's first unnamed attribute of a type.
func findAttribute(rec *mftRecord, typ uint32) *attribute {
	for i := range rec.attributes {
		if a := &rec.attributes[i]; a.typ == typ && !a.named {
			return a
		}
	}

	return nil
}

// run is a decoded data run: length clusters at lcn, or a sparse hole.
type run struct {
	lcn, length int64
	sparse      bool
}

// decodeRuns decodes an NTFS run list.
func decodeRuns(b []byte) ([]run, error) {
	var (
		runs []run
		lcn  int64
	)

	for i := 0; ; {
		if i >= len(b) {
			return nil, errors.New("the run list has no end marker")
		}

		h := b[i]
		if h == 0 {
			return runs, nil
		}

		lenSize, offSize := int(h&lowNibble), int(h>>bitsPerNibble)
		if lenSize == 0 || lenSize > 8 || offSize > 8 || i+1+lenSize+offSize > len(b) {
			return nil, errors.Errorf("corrupt run list at byte %v", i)
		}

		lengthBytes := b[i+1 : i+1+lenSize]
		offsetBytes := b[i+1+lenSize : i+1+lenSize+offSize]

		// little-endian: unsigned length, signed offset from the previous run
		length := int64(0)
		for _, x := range slices.Backward(lengthBytes) {
			length = length<<bitsPerByte | int64(x)
		}

		if length <= 0 {
			return nil, errors.Errorf("run of invalid length at byte %v", i)
		}

		r := run{length: length, sparse: offSize == 0}

		if offSize > 0 {
			delta := int64(int8(offsetBytes[offSize-1])) //nolint:gosec // the top byte holds the sign
			for _, x := range slices.Backward(offsetBytes[:offSize-1]) {
				delta = delta<<bitsPerByte | int64(x)
			}

			lcn += delta
			r.lcn = lcn
		}

		runs = append(runs, r)
		i += 1 + lenSize + offSize
	}
}

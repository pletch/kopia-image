package ntfsresize

import (
	"encoding/binary"
	"slices"

	"github.com/pkg/errors"
)

// Offsets in a non-resident attribute header.
const (
	nrStartVCN    = 0x10
	nrLastVCN     = 0x18
	nrRunsOffset  = 0x20
	nrAllocated   = 0x28
	nrDataSize    = 0x30
	nrInitialized = 0x38
	nrCompressed  = 0x40
)

// run is an extent of a non-resident attribute: length clusters at cluster
// lcn, or a sparse extent when sparse.
type run struct {
	length int64
	lcn    int64
	sparse bool
}

// nonResident is the header of a non-resident attribute.
type nonResident struct {
	startVCN, lastVCN int64
	allocated         int64
	dataSize          int64
	initialized       int64
	runs              []run
	runsEnd           int // in the attribute: the end of the encoded runs
}

func (r *record) nonResident(a attribute) (*nonResident, error) {
	if !a.nonResident {
		return nil, errors.Errorf("MFT record %v: attribute %#x is resident", r.number, a.typ)
	}

	b := r.buf[a.offset : a.offset+a.length]
	le := binary.LittleEndian

	nr := &nonResident{
		startVCN:    int64(le.Uint64(b[nrStartVCN:])),    //nolint:gosec
		lastVCN:     int64(le.Uint64(b[nrLastVCN:])),     //nolint:gosec
		allocated:   int64(le.Uint64(b[nrAllocated:])),   //nolint:gosec
		dataSize:    int64(le.Uint64(b[nrDataSize:])),    //nolint:gosec
		initialized: int64(le.Uint64(b[nrInitialized:])), //nolint:gosec
	}

	runsOff := int(le.Uint16(b[nrRunsOffset:]))
	if runsOff < nonResidentHeaderSize || runsOff >= a.length {
		return nil, errors.Errorf("MFT record %v: attribute %#x has its runs at %v", r.number, a.typ, runsOff)
	}

	runs, n, err := decodeRuns(b[runsOff:])
	if err != nil {
		return nil, errors.Wrapf(err, "MFT record %v: attribute %#x", r.number, a.typ)
	}

	nr.runs = runs
	nr.runsEnd = runsOff + n

	return nr, nil
}

// decodeRuns decodes a run list and returns its runs and the length of its
// encoding, including the terminating zero.
func decodeRuns(b []byte) ([]run, int, error) {
	var (
		out []run
		lcn int64
	)

	for i := 0; ; {
		if i >= len(b) {
			return nil, 0, errors.New("run list does not end")
		}

		h := b[i]
		if h == 0 {
			return out, i + 1, nil
		}

		lenBytes, offBytes := int(h&0x0F), int(h>>4) //nolint:mnd
		if lenBytes == 0 || lenBytes > 8 || offBytes > 8 || i+1+lenBytes+offBytes > len(b) {
			return nil, 0, errors.Errorf("invalid run header %#x", h)
		}

		length := signedLE(b[i+1 : i+1+lenBytes])
		if length <= 0 {
			return nil, 0, errors.Errorf("invalid run length %v", length)
		}

		r := run{length: length, sparse: offBytes == 0}

		if !r.sparse {
			lcn += signedLE(b[i+1+lenBytes : i+1+lenBytes+offBytes])
			if lcn < 0 {
				return nil, 0, errors.Errorf("invalid run cluster %v", lcn)
			}

			r.lcn = lcn
		}

		out = append(out, r)
		i += 1 + lenBytes + offBytes
	}
}

// encodeRuns encodes runs as a run list ending with a zero.
func encodeRuns(runs []run) []byte {
	var (
		out  []byte
		prev int64
	)

	for _, r := range runs {
		length := encodeSigned(r.length)

		var off []byte
		if !r.sparse {
			off = encodeSigned(r.lcn - prev)
			prev = r.lcn
		}

		out = append(out, byte(len(off)<<4|len(length))) //nolint:gosec
		out = append(out, length...)
		out = append(out, off...)
	}

	return append(out, 0)
}

func signedLE(b []byte) int64 {
	var v int64

	for _, x := range slices.Backward(b) {
		v = v<<8 | int64(x) //nolint:mnd
	}

	// sign-extend
	shift := 64 - 8*len(b) //nolint:mnd

	return v << shift >> shift
}

// encodeSigned encodes v in the fewest little-endian bytes that hold it as
// a signed number.
func encodeSigned(v int64) []byte {
	var out []byte

	for {
		out = append(out, byte(v)) //nolint:gosec
		v >>= 8

		last := out[len(out)-1]
		if (v == 0 && last&0x80 == 0) || (v == -1 && last&0x80 != 0) {
			return out
		}
	}
}

// clusters returns the number of clusters the runs cover.
func clusters(runs []run) int64 {
	var n int64

	for _, r := range runs {
		n += r.length
	}

	return n
}

// truncateRuns returns the runs covering the first n clusters.
func truncateRuns(runs []run, n int64) []run {
	var out []run

	for _, r := range runs {
		if n <= 0 {
			break
		}

		r.length = min(r.length, n)
		n -= r.length
		out = append(out, r)
	}

	return out
}

// Package ntfsresize shrinks an NTFS volume whose allocated clusters all lie
// below its new end, so that an image can be restored to a smaller partition.
// It moves no data: it changes the size of the volume in its boot sectors,
// the size of the $Bitmap and $BadClus metadata files, and marks the volume
// for checking, as ntfsresize of ntfs-3g does for such a shrink.
package ntfsresize

import (
	"encoding/binary"
	"io"
	"math"
	"math/bits"

	"github.com/pkg/errors"
)

// MFT records of the metadata files that a shrink changes.
const (
	recordMFT     = 0
	recordVolume  = 3
	recordBitmap  = 6
	recordBadClus = 8

	// recordsMirrored are the records that $MFTMirr holds a copy of.
	recordsMirrored = 4
)

// Attribute types and flags.
const (
	attrVolumeInformation = 0x70
	attrData              = 0x80
	attrEnd               = 0xFFFFFFFF

	attrFlagCompressed = 0x0001
	attrFlagSparse     = 0x8000

	volumeFlagDirty = 0x0001
)

const (
	bootSectorSize = 512

	// fixupStride is the span of each update sequence entry, 512 bytes
	// whatever the sector size.
	fixupStride = 512
	usnSize     = 2

	recordHeaderSize      = 0x30
	residentHeaderSize    = 0x18
	nonResidentHeaderSize = 0x40

	// bitmapRounding is what the size of $Bitmap is a multiple of.
	bitmapRounding = 8

	maxRecordSize = 64 << 10
	maxSectorSize = 4096
	maxCluster    = 2 << 20
)

// ErrNotNTFS is returned for a volume without an NTFS boot sector.
var ErrNotNTFS = errors.New("not an NTFS volume")

// Device is a volume, or a file holding one, read and written in place.
type Device interface {
	io.ReaderAt
	io.WriterAt
}

// Boot is the geometry from an NTFS boot sector.
type Boot struct {
	BytesPerSector int64
	ClusterSize    int64

	// TotalSectors is the size of the volume in sectors, not counting the
	// backup boot sector, which follows it in the last sector of the
	// partition.
	TotalSectors int64

	MFTCluster     int64
	MFTMirrCluster int64
	RecordSize     int64
}

// TotalClusters returns the number of clusters of the volume.
func (b *Boot) TotalClusters() int64 {
	return b.TotalSectors * b.BytesPerSector / b.ClusterSize
}

// ParseBoot decodes and checks an NTFS boot sector.
func ParseBoot(sector []byte) (*Boot, error) {
	if len(sector) < bootSectorSize || string(sector[3:11]) != "NTFS    " || sector[510] != 0x55 || sector[511] != 0xAA {
		return nil, ErrNotNTFS
	}

	le := binary.LittleEndian

	b := &Boot{
		BytesPerSector: int64(le.Uint16(sector[0x0B:])),
		TotalSectors:   int64(le.Uint64(sector[0x28:])), //nolint:gosec
		MFTCluster:     int64(le.Uint64(sector[0x30:])), //nolint:gosec
		MFTMirrCluster: int64(le.Uint64(sector[0x38:])), //nolint:gosec
	}

	if b.BytesPerSector < bootSectorSize || b.BytesPerSector > maxSectorSize || bits.OnesCount64(uint64(b.BytesPerSector)) != 1 {
		return nil, errors.Errorf("invalid sector size %v", b.BytesPerSector)
	}

	// sectors per cluster, or for clusters of 64 KiB and more a negative
	// power of two
	spc := int64(sector[0x0D])
	if spc > 0x80 { //nolint:mnd
		spc = 1 << (256 - spc) //nolint:mnd
	}

	b.ClusterSize = spc * b.BytesPerSector
	b.RecordSize = clusterOrPowerOfTwo(int8(sector[0x40]), b.ClusterSize) //nolint:gosec

	if err := b.validate(spc); err != nil {
		return nil, err
	}

	return b, nil
}

func (b *Boot) validate(sectorsPerCluster int64) error {
	if sectorsPerCluster <= 0 || sectorsPerCluster&(sectorsPerCluster-1) != 0 || b.ClusterSize <= 0 || b.ClusterSize > maxCluster {
		return errors.Errorf("invalid cluster size: %v sectors", sectorsPerCluster)
	}

	if b.RecordSize < fixupStride || b.RecordSize > maxRecordSize || b.RecordSize%fixupStride != 0 {
		return errors.Errorf("invalid MFT record size %v", b.RecordSize)
	}

	// TotalClusters multiplies TotalSectors by BytesPerSector
	if b.TotalSectors <= 0 || b.TotalSectors > math.MaxInt64/b.BytesPerSector {
		return errors.Errorf("invalid geometry: %v sectors", b.TotalSectors)
	}

	clusters := b.TotalClusters()

	if b.MFTCluster <= 0 || b.MFTCluster >= clusters || b.MFTMirrCluster <= 0 || b.MFTMirrCluster >= clusters {
		return errors.Errorf("invalid geometry: %v sectors, $MFT at cluster %v, $MFTMirr at %v", b.TotalSectors, b.MFTCluster, b.MFTMirrCluster)
	}

	return nil
}

// clusterOrPowerOfTwo decodes a size in clusters, or when negative, 2^-v bytes.
func clusterOrPowerOfTwo(v int8, clusterSize int64) int64 {
	if v < 0 {
		return 1 << uint(-v)
	}

	return int64(v) * clusterSize
}

// record is an MFT record read from a volume, with its fixups applied.
type record struct {
	number int64
	offset int64 // on the volume
	buf    []byte
}

func readBoot(dev io.ReaderAt) (*Boot, []byte, error) {
	sector := make([]byte, bootSectorSize)

	if _, err := dev.ReadAt(sector, 0); err != nil {
		return nil, nil, errors.Wrap(err, "unable to read the boot sector")
	}

	b, err := ParseBoot(sector)

	return b, sector, err
}

// readRecord reads one of the first records of the MFT, which lie in the
// first extent of the MFT; checkFirstExtent checks that they do.
func readRecord(dev io.ReaderAt, b *Boot, base, n int64) (*record, error) {
	r := &record{number: n, offset: base + n*b.RecordSize, buf: make([]byte, b.RecordSize)}

	if _, err := dev.ReadAt(r.buf, r.offset); err != nil {
		return nil, errors.Wrapf(err, "unable to read MFT record %v", n)
	}

	if string(r.buf[0:4]) != "FILE" {
		return nil, errors.Errorf("MFT record %v has no FILE signature", n)
	}

	if err := applyFixups(r.buf); err != nil {
		return nil, errors.Wrapf(err, "MFT record %v", n)
	}

	le := binary.LittleEndian

	if alloc := int64(le.Uint32(r.buf[0x1C:])); alloc != b.RecordSize {
		return nil, errors.Errorf("MFT record %v is %v bytes, the boot sector says %v", n, alloc, b.RecordSize)
	}

	if le.Uint16(r.buf[4:]) >= recordHeaderSize {
		if got := int64(le.Uint32(r.buf[0x2C:])); got != n {
			return nil, errors.Errorf("MFT record %v holds record number %v", n, got)
		}
	}

	return r, nil
}

// write writes the record back with its fixups, to the MFT and, for the
// records that $MFTMirr holds, to the mirror.
func (r *record) write(dev io.WriterAt, b *Boot) error {
	out := append([]byte(nil), r.buf...)

	if err := unapplyFixups(out); err != nil {
		return errors.Wrapf(err, "MFT record %v", r.number)
	}

	if _, err := dev.WriteAt(out, r.offset); err != nil {
		return errors.Wrapf(err, "unable to write MFT record %v", r.number)
	}

	if r.number < recordsMirrored {
		if _, err := dev.WriteAt(out, b.MFTMirrCluster*b.ClusterSize+r.number*b.RecordSize); err != nil {
			return errors.Wrapf(err, "unable to write the mirror of MFT record %v", r.number)
		}
	}

	return nil
}

func applyFixups(buf []byte) error {
	le := binary.LittleEndian
	usaOff := int(le.Uint16(buf[4:]))
	usaCount := int(le.Uint16(buf[6:]))

	if usaCount != len(buf)/fixupStride+1 || usaOff < 0x28 || usaOff+usnSize*usaCount > len(buf) {
		return errors.Errorf("invalid update sequence array (offset %v, %v entries)", usaOff, usaCount)
	}

	usn := buf[usaOff : usaOff+usnSize]

	for i := 1; i < usaCount; i++ {
		end := i*fixupStride - usnSize

		if buf[end] != usn[0] || buf[end+1] != usn[1] {
			return errors.Errorf("torn write: sector %v of the record is from another write", i-1)
		}

		copy(buf[end:end+usnSize], buf[usaOff+usnSize*i:])
	}

	return nil
}

// unapplyFixups saves the last two bytes of every stride in the update
// sequence array and puts the update sequence number in their place.
func unapplyFixups(buf []byte) error {
	le := binary.LittleEndian
	usaOff := int(le.Uint16(buf[4:]))
	usaCount := int(le.Uint16(buf[6:]))

	if usaCount != len(buf)/fixupStride+1 || usaOff < 0x28 || usaOff+usnSize*usaCount > len(buf) {
		return errors.Errorf("invalid update sequence array (offset %v, %v entries)", usaOff, usaCount)
	}

	for i := 1; i < usaCount; i++ {
		end := i*fixupStride - usnSize

		copy(buf[usaOff+usnSize*i:], buf[end:end+usnSize])
		copy(buf[end:end+usnSize], buf[usaOff:usaOff+usnSize])
	}

	return nil
}

// attribute is the location of an attribute in a record.
type attribute struct {
	typ    uint32
	offset int // in the record
	length int

	nonResident bool
	flags       uint16
	name        string
}

// attributes lists the attributes of a record.
func (r *record) attributes() ([]attribute, error) {
	le := binary.LittleEndian
	used := int(le.Uint32(r.buf[0x18:]))
	off := int(le.Uint16(r.buf[0x14:]))

	if used > len(r.buf) || off < 0x28 || off >= used {
		return nil, errors.Errorf("MFT record %v has an invalid header", r.number)
	}

	var out []attribute

	for {
		if off+4 > used {
			return nil, errors.Errorf("MFT record %v: attribute list does not end", r.number)
		}

		typ := le.Uint32(r.buf[off:])
		if typ == attrEnd {
			return out, nil
		}

		if off+residentHeaderSize > used {
			return nil, errors.Errorf("MFT record %v: truncated attribute at %v", r.number, off)
		}

		length := int(le.Uint32(r.buf[off+4:]))
		if length < residentHeaderSize || length%8 != 0 || off+length > used {
			return nil, errors.Errorf("MFT record %v: attribute at %v has invalid length %v", r.number, off, length)
		}

		a := attribute{offset: off, length: length, nonResident: r.buf[off+8] != 0, flags: le.Uint16(r.buf[off+0x0C:])}

		if nameLen := int(r.buf[off+9]); nameLen > 0 {
			nameOff := int(le.Uint16(r.buf[off+0x0A:]))
			if nameOff+2*nameLen > length {
				return nil, errors.Errorf("MFT record %v: attribute at %v has an invalid name", r.number, off)
			}

			a.name = decodeName(r.buf[off+nameOff : off+nameOff+2*nameLen])
		}

		if a.nonResident && length < nonResidentHeaderSize {
			return nil, errors.Errorf("MFT record %v: non-resident attribute at %v is %v bytes", r.number, off, length)
		}

		a.typ = typ
		out = append(out, a)
		off += length
	}
}

// find returns the attribute of a type and name, or an error if the record
// has none or several.
func (r *record) find(typ uint32, name string) (attribute, error) {
	attrs, err := r.attributes()
	if err != nil {
		return attribute{}, err
	}

	var (
		found attribute
		n     int
	)

	for _, a := range attrs {
		if a.typ == typ && a.name == name {
			found = a
			n++
		}
	}

	if n != 1 {
		return attribute{}, errors.Errorf("MFT record %v has %v attributes of type %#x named %q, expected one", r.number, n, typ, name)
	}

	return found, nil
}

func decodeName(b []byte) string {
	runes := make([]rune, 0, len(b)/2) //nolint:mnd

	for i := 0; i+1 < len(b); i += 2 {
		runes = append(runes, rune(binary.LittleEndian.Uint16(b[i:])))
	}

	return string(runes)
}

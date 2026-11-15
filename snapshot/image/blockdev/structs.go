// Package blockdev reads disks, partitions and volume shadow copies as block
// devices for kopia image.
//
// The decoders of the Windows disk I/O control structures are platform
// independent, so they are tested on every platform; the device access itself
// is Windows-only.
package blockdev

import (
	"encoding/binary"
	"fmt"
	"unicode/utf16"

	"github.com/pkg/errors"

	"github.com/kopia/kopia/snapshot/image"
)

// Partition styles from the PARTITION_STYLE enumeration.
const (
	partitionStyleMBR = 0
	partitionStyleGPT = 1
	partitionStyleRAW = 2
)

// Layout of DRIVE_LAYOUT_INFORMATION_EX and PARTITION_INFORMATION_EX on
// 64-bit Windows.
const (
	driveLayoutHeaderSize = 48
	driveLayoutDiskIDOff  = 8

	partitionInfoSize        = 144
	partitionInfoStyleOff    = 0
	partitionInfoOffsetOff   = 8
	partitionInfoLengthOff   = 16
	partitionInfoNumberOff   = 24
	partitionInfoTypeOff     = 32
	partitionInfoIDOff       = 48
	partitionInfoAttrsOff    = 64
	partitionInfoNameOff     = 72
	partitionInfoNameUTF16Sz = 36

	guidSize = 16

	diskGeometryExMinSize    = 32
	diskGeometrySectorSzOff  = 20
	diskGeometryDiskSizeOff  = 24
	lengthInfoSize           = 8
	diskExtentsHeaderSize    = 8
	diskExtentSize           = 24
	diskExtentOffsetFieldOff = 8
	diskExtentLengthFieldOff = 16
)

const zeroGUID = "00000000-0000-0000-0000-000000000000"

// ErrNotGPT is returned for disks that do not use a GUID partition table.
var ErrNotGPT = errors.Wrap(image.ErrUnsupportedLayout, "disk does not use a GUID partition table")

// DiskExtent is the part of a physical disk that holds a volume.
type DiskExtent struct {
	DiskNumber uint32
	Offset     int64
	Length     int64
}

// decodeGUID decodes a Windows GUID structure, whose first three fields are
// little-endian, into canonical form.
func decodeGUID(b []byte) string {
	return fmt.Sprintf("%08x-%04x-%04x-%x-%x",
		binary.LittleEndian.Uint32(b[0:4]),
		binary.LittleEndian.Uint16(b[4:6]),
		binary.LittleEndian.Uint16(b[6:8]),
		b[8:10], b[10:16])
}

// decodeDriveLayout decodes the output of IOCTL_DISK_GET_DRIVE_LAYOUT_EX for a
// GPT disk into the disk GUID and the partitions in use.
func decodeDriveLayout(b []byte) (diskGUID string, partitions []image.Partition, err error) {
	if len(b) < driveLayoutHeaderSize {
		return "", nil, errors.Errorf("drive layout is %v bytes, too short", len(b))
	}

	style := binary.LittleEndian.Uint32(b[0:4])
	if style != partitionStyleGPT {
		return "", nil, errors.Wrapf(ErrNotGPT, "partition style %v", style)
	}

	count := int(binary.LittleEndian.Uint32(b[4:8]))
	if len(b) < driveLayoutHeaderSize+count*partitionInfoSize {
		return "", nil, errors.Errorf("drive layout is %v bytes, too short for %v partitions", len(b), count)
	}

	diskGUID = decodeGUID(b[driveLayoutDiskIDOff : driveLayoutDiskIDOff+guidSize])

	for i := range count {
		e := b[driveLayoutHeaderSize+i*partitionInfoSize:][:partitionInfoSize]

		if s := binary.LittleEndian.Uint32(e[partitionInfoStyleOff:]); s != partitionStyleGPT {
			return "", nil, errors.Wrapf(ErrNotGPT, "partition entry %v has style %v", i, s)
		}

		typeGUID := decodeGUID(e[partitionInfoTypeOff : partitionInfoTypeOff+guidSize])
		if typeGUID == zeroGUID {
			continue
		}

		partitions = append(partitions, image.Partition{
			Number:     int(binary.LittleEndian.Uint32(e[partitionInfoNumberOff:])),
			TypeGUID:   typeGUID,
			UniqueGUID: decodeGUID(e[partitionInfoIDOff : partitionInfoIDOff+guidSize]),
			Offset:     int64(binary.LittleEndian.Uint64(e[partitionInfoOffsetOff:])), //nolint:gosec
			Length:     int64(binary.LittleEndian.Uint64(e[partitionInfoLengthOff:])), //nolint:gosec
			Name:       decodeUTF16Name(e[partitionInfoNameOff : partitionInfoNameOff+2*partitionInfoNameUTF16Sz]),
			Attributes: binary.LittleEndian.Uint64(e[partitionInfoAttrsOff:]),
		})
	}

	return diskGUID, partitions, nil
}

func decodeUTF16Name(b []byte) string {
	u := make([]uint16, 0, len(b)/2) //nolint:mnd

	for i := 0; i+1 < len(b); i += 2 {
		c := binary.LittleEndian.Uint16(b[i:])
		if c == 0 {
			break
		}

		u = append(u, c)
	}

	return string(utf16.Decode(u))
}

// decodeGeometry decodes the logical sector size and disk size from the
// output of IOCTL_DISK_GET_DRIVE_GEOMETRY_EX.
func decodeGeometry(b []byte) (sectorSize, diskSize int64, err error) {
	if len(b) < diskGeometryExMinSize {
		return 0, 0, errors.Errorf("disk geometry is %v bytes, too short", len(b))
	}

	return int64(binary.LittleEndian.Uint32(b[diskGeometrySectorSzOff:])),
		int64(binary.LittleEndian.Uint64(b[diskGeometryDiskSizeOff:])), //nolint:gosec
		nil
}

// decodeLength decodes the output of IOCTL_DISK_GET_LENGTH_INFO.
func decodeLength(b []byte) (int64, error) {
	if len(b) < lengthInfoSize {
		return 0, errors.Errorf("length info is %v bytes, too short", len(b))
	}

	return int64(binary.LittleEndian.Uint64(b)), nil //nolint:gosec
}

// decodeDiskExtents decodes the output of IOCTL_VOLUME_GET_VOLUME_DISK_EXTENTS.
func decodeDiskExtents(b []byte) ([]DiskExtent, error) {
	if len(b) < diskExtentsHeaderSize {
		return nil, errors.Errorf("disk extents are %v bytes, too short", len(b))
	}

	count := int(binary.LittleEndian.Uint32(b))
	if len(b) < diskExtentsHeaderSize+count*diskExtentSize {
		return nil, errors.Errorf("disk extents are %v bytes, too short for %v extents", len(b), count)
	}

	out := make([]DiskExtent, count)

	for i := range out {
		e := b[diskExtentsHeaderSize+i*diskExtentSize:]
		out[i] = DiskExtent{
			DiskNumber: binary.LittleEndian.Uint32(e),
			Offset:     int64(binary.LittleEndian.Uint64(e[diskExtentOffsetFieldOff:])), //nolint:gosec
			Length:     int64(binary.LittleEndian.Uint64(e[diskExtentLengthFieldOff:])), //nolint:gosec
		}
	}

	return out, nil
}

// buildLayout assembles and validates a layout from decoded disk structures.
func buildLayout(diskGUID string, sectorSize, diskSize int64, partitions []image.Partition) (*image.Layout, error) {
	l := &image.Layout{
		DiskGUID:   diskGUID,
		SectorSize: sectorSize,
		DiskSize:   diskSize,
		Partitions: partitions,
	}

	if err := l.Validate(); err != nil {
		return nil, errors.Wrap(err, "invalid disk layout")
	}

	if err := l.CheckSupported(); err != nil {
		return nil, errors.Wrap(err, "disk layout")
	}

	return l, nil
}

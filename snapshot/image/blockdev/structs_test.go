package blockdev

import (
	"bytes"
	"encoding/binary"
	"testing"
	"unicode/utf16"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/snapshot/image"
)

// encodeGUID is the inverse of decodeGUID.
func encodeGUID(t *testing.T, s string) []byte {
	t.Helper()

	u, err := uuid.Parse(s)
	require.NoError(t, err)

	b := make([]byte, guidSize)
	binary.LittleEndian.PutUint32(b[0:], binary.BigEndian.Uint32(u[0:4]))
	binary.LittleEndian.PutUint16(b[4:], binary.BigEndian.Uint16(u[4:6]))
	binary.LittleEndian.PutUint16(b[6:], binary.BigEndian.Uint16(u[6:8]))
	copy(b[8:], u[8:])

	return b
}

func encodePartition(t *testing.T, p image.Partition) []byte {
	t.Helper()

	e := make([]byte, partitionInfoSize)
	binary.LittleEndian.PutUint32(e[partitionInfoStyleOff:], partitionStyleGPT)
	binary.LittleEndian.PutUint64(e[partitionInfoOffsetOff:], uint64(p.Offset))
	binary.LittleEndian.PutUint64(e[partitionInfoLengthOff:], uint64(p.Length))
	binary.LittleEndian.PutUint32(e[partitionInfoNumberOff:], uint32(p.Number))

	if p.TypeGUID != "" {
		copy(e[partitionInfoTypeOff:], encodeGUID(t, p.TypeGUID))
		copy(e[partitionInfoIDOff:], encodeGUID(t, p.UniqueGUID))
	}

	binary.LittleEndian.PutUint64(e[partitionInfoAttrsOff:], p.Attributes)

	for i, c := range utf16.Encode([]rune(p.Name)) {
		binary.LittleEndian.PutUint16(e[partitionInfoNameOff+2*i:], c)
	}

	return e
}

func encodeDriveLayout(t *testing.T, style uint32, diskGUID string, parts []image.Partition) []byte {
	t.Helper()

	header := make([]byte, driveLayoutHeaderSize)
	binary.LittleEndian.PutUint32(header[0:], style)
	binary.LittleEndian.PutUint32(header[4:], uint32(len(parts)))
	copy(header[driveLayoutDiskIDOff:], encodeGUID(t, diskGUID))

	var buf bytes.Buffer

	buf.Write(header)

	for _, p := range parts {
		buf.Write(encodePartition(t, p))
	}

	return buf.Bytes()
}

const testDiskGUID = "0f6ab3f2-1c3d-4e5f-8a9b-0c1d2e3f4a5b"

func testPartitions() []image.Partition {
	return []image.Partition{
		{
			Number: 1, TypeGUID: image.PartitionTypeEFISystem, UniqueGUID: "11111111-2222-4333-8444-555555555551",
			Offset: 1 << 20, Length: 100 << 20, Name: "EFI system partition", Attributes: 1 << 63,
		},
		{
			Number: 2, TypeGUID: image.PartitionTypeBasicData, UniqueGUID: "11111111-2222-4333-8444-555555555552",
			Offset: 101 << 20, Length: 1 << 30, Name: "Basic data partition",
		},
	}
}

func TestDecodeGUID_windowsByteOrder(t *testing.T) {
	// the EFI system partition type GUID as stored on disk
	b := []byte{0x28, 0x73, 0x2a, 0xc1, 0x1f, 0xf8, 0xd2, 0x11, 0xba, 0x4b, 0x00, 0xa0, 0xc9, 0x3e, 0xc9, 0x3b}
	require.Equal(t, image.PartitionTypeEFISystem, decodeGUID(b))
	require.Equal(t, b, encodeGUID(t, image.PartitionTypeEFISystem))
}

func TestDecodeDriveLayout(t *testing.T) {
	parts := testPartitions()
	unused := image.Partition{Number: 3}

	diskGUID, got, err := decodeDriveLayout(encodeDriveLayout(t, partitionStyleGPT, testDiskGUID, append(parts, unused)))
	require.NoError(t, err)
	require.Equal(t, testDiskGUID, diskGUID)
	require.Equal(t, parts, got)
}

func TestDecodeDriveLayout_notGPT(t *testing.T) {
	for _, style := range []uint32{partitionStyleMBR, partitionStyleRAW} {
		_, _, err := decodeDriveLayout(encodeDriveLayout(t, style, testDiskGUID, nil))
		require.ErrorIs(t, err, ErrNotGPT)
		require.ErrorIs(t, err, image.ErrUnsupportedLayout)
	}
}

func TestDecodeDriveLayout_truncated(t *testing.T) {
	b := encodeDriveLayout(t, partitionStyleGPT, testDiskGUID, testPartitions())

	_, _, err := decodeDriveLayout(b[:len(b)-1])
	require.ErrorContains(t, err, "too short for 2 partitions")

	_, _, err = decodeDriveLayout(b[:driveLayoutHeaderSize-1])
	require.ErrorContains(t, err, "too short")
}

func TestDecodeGeometry(t *testing.T) {
	b := make([]byte, diskGeometryExMinSize)
	binary.LittleEndian.PutUint32(b[diskGeometrySectorSzOff:], 4096)
	binary.LittleEndian.PutUint64(b[diskGeometryDiskSizeOff:], 1<<40)

	ss, size, err := decodeGeometry(b)
	require.NoError(t, err)
	require.Equal(t, int64(4096), ss)
	require.Equal(t, int64(1<<40), size)

	_, _, err = decodeGeometry(b[:diskGeometryExMinSize-1])
	require.Error(t, err)
}

func TestDecodeLength(t *testing.T) {
	b := binary.LittleEndian.AppendUint64(nil, 123456789)

	n, err := decodeLength(b)
	require.NoError(t, err)
	require.Equal(t, int64(123456789), n)

	_, err = decodeLength(b[:7])
	require.Error(t, err)
}

func TestDecodeDiskExtents(t *testing.T) {
	b := make([]byte, diskExtentsHeaderSize+2*diskExtentSize)
	binary.LittleEndian.PutUint32(b, 2)

	for i, e := range []DiskExtent{{0, 1 << 20, 1 << 30}, {3, 2 << 20, 5 << 30}} {
		o := b[diskExtentsHeaderSize+i*diskExtentSize:]
		binary.LittleEndian.PutUint32(o, e.DiskNumber)
		binary.LittleEndian.PutUint64(o[diskExtentOffsetFieldOff:], uint64(e.Offset))
		binary.LittleEndian.PutUint64(o[diskExtentLengthFieldOff:], uint64(e.Length))
	}

	got, err := decodeDiskExtents(b)
	require.NoError(t, err)
	require.Equal(t, []DiskExtent{{0, 1 << 20, 1 << 30}, {3, 2 << 20, 5 << 30}}, got)

	_, err = decodeDiskExtents(b[:len(b)-1])
	require.Error(t, err)
}

func TestBuildLayout(t *testing.T) {
	l, err := buildLayout(testDiskGUID, 512, 4<<30, testPartitions())
	require.NoError(t, err)
	require.Len(t, l.Partitions, 2)

	parts := testPartitions()
	parts[1].TypeGUID = "af9b60a0-1431-4f62-bc68-3311714a69ad"

	_, err = buildLayout(testDiskGUID, 512, 4<<30, parts)
	require.ErrorIs(t, err, image.ErrUnsupportedLayout)

	_, err = buildLayout(testDiskGUID, 512, 1<<20, testPartitions())
	require.ErrorContains(t, err, "invalid disk layout")
}

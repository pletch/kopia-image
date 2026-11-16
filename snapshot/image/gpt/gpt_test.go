package gpt

import (
	"encoding/binary"
	"hash/crc32"
	"io"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/snapshot/image"
)

// memDisk is an in-memory disk.
type memDisk []byte

func (m memDisk) WriteAt(p []byte, off int64) (int, error) {
	if off+int64(len(p)) > int64(len(m)) {
		return 0, io.ErrShortWrite
	}

	return copy(m[off:], p), nil
}

func (m memDisk) ReadAt(p []byte, off int64) (int, error) {
	if off >= int64(len(m)) {
		return 0, io.EOF
	}

	n := copy(p, m[off:])
	if n < len(p) {
		return n, io.EOF
	}

	return n, nil
}

func testLayout(sectorSize int64) *image.Layout {
	return &image.Layout{
		DiskGUID:   "0f6ab3f2-1c3d-4e5f-8a9b-0c1d2e3f4a5b",
		SectorSize: sectorSize,
		DiskSize:   64 << 20,
		Partitions: []image.Partition{
			{Number: 1, TypeGUID: image.PartitionTypeEFISystem, UniqueGUID: "11111111-2222-4333-8444-555555555551", Offset: 1 << 20, Length: 8 << 20, Name: "EFI system partition", Attributes: 1 << 63},
			{Number: 2, TypeGUID: image.PartitionTypeMSR, UniqueGUID: "11111111-2222-4333-8444-555555555552", Offset: 9 << 20, Length: 16 << 20, Name: "Microsoft reserved partition"},
			{Number: 4, TypeGUID: image.PartitionTypeBasicData, UniqueGUID: "11111111-2222-4333-8444-555555555554", Offset: 25 << 20, Length: 30 << 20, Name: "Basic data ünïcode", Attributes: 1<<63 | 1},
		},
	}
}

func TestWriteRead_roundTrip(t *testing.T) {
	for _, sectorSize := range []int64{512, 4096} {
		for _, diskSize := range []int64{64 << 20, 128 << 20} {
			l := testLayout(sectorSize)
			disk := make(memDisk, diskSize)

			require.NoError(t, Write(disk, l, diskSize))

			got, err := Read(disk, sectorSize)
			require.NoError(t, err)
			require.Equal(t, l.DiskGUID, got.DiskGUID)
			require.Equal(t, l.Partitions, got.Partitions, "sector %v disk %v", sectorSize, diskSize)

			verifyBackup(t, disk, sectorSize, diskSize)
		}
	}
}

// verifyBackup checks the backup header at the last LBA and its entries.
func verifyBackup(t *testing.T, disk memDisk, sectorSize, diskSize int64) {
	t.Helper()

	lastLBA := diskSize/sectorSize - 1
	hdr := disk[lastLBA*sectorSize:][:headerSize]
	primary := disk[sectorSize:][:headerSize]

	require.Equal(t, signature, string(hdr[0:8]))
	require.Equal(t, uint64(lastLBA), binary.LittleEndian.Uint64(hdr[24:]), "backup MyLBA")
	require.Equal(t, uint64(1), binary.LittleEndian.Uint64(hdr[32:]), "backup AlternateLBA")
	require.Equal(t, uint64(lastLBA), binary.LittleEndian.Uint64(primary[32:]), "primary AlternateLBA")

	entriesLBA := int64(binary.LittleEndian.Uint64(hdr[72:]))
	entries := disk[entriesLBA*sectorSize:][:entryArrayBytes]
	require.Equal(t, binary.LittleEndian.Uint32(hdr[88:]), crc32.ChecksumIEEE(entries))
	require.Equal(t, disk[2*sectorSize:][:entryArrayBytes], entries, "backup entries equal primary entries")

	// the header CRC covers the header with the CRC field zeroed
	check := append([]byte(nil), hdr...)
	binary.LittleEndian.PutUint32(check[16:], 0)
	require.Equal(t, binary.LittleEndian.Uint32(hdr[16:]), crc32.ChecksumIEEE(check))

	// the usable area matches between headers and leaves room for the entry arrays
	entrySectors := (int64(entryArrayBytes) + sectorSize - 1) / sectorSize
	require.Equal(t, uint64(2+entrySectors), binary.LittleEndian.Uint64(primary[40:]))
	require.Equal(t, uint64(lastLBA-1-entrySectors), binary.LittleEndian.Uint64(primary[48:]))
	require.Equal(t, primary[40:56], hdr[40:56])
}

func TestWrite_protectiveMBR(t *testing.T) {
	disk := make(memDisk, 64<<20)
	require.NoError(t, Write(disk, testLayout(512), int64(len(disk))))

	require.Equal(t, []byte{0x55, 0xaa}, []byte(disk[510:512]))
	require.Equal(t, byte(0xee), disk[446+4])
	require.Equal(t, uint32(1), binary.LittleEndian.Uint32(disk[446+8:]))
	require.Equal(t, uint32(len(disk)/512-1), binary.LittleEndian.Uint32(disk[446+12:]))
}

func TestWrite_errors(t *testing.T) {
	l := testLayout(512)

	// the last partition does not fit before the backup GPT of a smaller disk
	require.ErrorContains(t, Write(make(memDisk, 55<<20), l, 55<<20), "outside the usable area")

	l = testLayout(512)
	l.DiskGUID = "x"
	require.ErrorContains(t, Write(make(memDisk, 64<<20), l, 64<<20), "invalid layout")
}

func TestRead_invalid(t *testing.T) {
	disk := make(memDisk, 64<<20)

	_, err := Read(disk, 512)
	require.ErrorIs(t, err, ErrNoGPT)

	require.NoError(t, Write(disk, testLayout(512), int64(len(disk))))

	disk[2*512+10] ^= 0xff // corrupt an entry

	_, err = Read(disk, 512)
	require.ErrorIs(t, err, ErrNoGPT)
	require.ErrorContains(t, err, "entry array checksum")

	require.NoError(t, Write(disk, testLayout(512), int64(len(disk))))

	disk[512+40] ^= 0xff // corrupt the header

	_, err = Read(disk, 512)
	require.ErrorContains(t, err, "header checksum")
}

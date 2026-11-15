package ntfs

import (
	"encoding/binary"
	"testing"
	"unicode/utf16"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/snapshot/image"
)

func TestDecodeVolumeData(t *testing.T) {
	b := make([]byte, volumeDataSize)
	binary.LittleEndian.PutUint64(b[volumeDataSerialOff:], 0x8a1c2e3f4b5d6a7e)
	binary.LittleEndian.PutUint64(b[volumeDataTotalClusters:], 1000)
	binary.LittleEndian.PutUint64(b[volumeDataFreeClusters:], 400)
	binary.LittleEndian.PutUint32(b[volumeDataBytesPerSector:], 512)
	binary.LittleEndian.PutUint32(b[volumeDataBytesPerClust:], 4096)
	binary.LittleEndian.PutUint64(b[volumeDataMftStartLcn:], 786432)

	vd, err := decodeVolumeData(b)
	require.NoError(t, err)
	require.Equal(t, VolumeData{
		SerialNumber: 0x8a1c2e3f4b5d6a7e, TotalClusters: 1000, FreeClusters: 400,
		BytesPerSector: 512, ClusterSize: 4096, MftStartLcn: 786432,
	}, vd)

	_, err = decodeVolumeData(b[:volumeDataSize-1])
	require.Error(t, err)

	binary.LittleEndian.PutUint32(b[volumeDataBytesPerClust:], 1000)

	_, err = decodeVolumeData(b)
	require.ErrorContains(t, err, "invalid volume data")
}

func TestDecodeBitmapChunk(t *testing.T) {
	b := binary.LittleEndian.AppendUint64(nil, 64)
	b = binary.LittleEndian.AppendUint64(b, 100)
	b = append(b, 0xff, 0x0f)

	start, bitmap, err := decodeBitmapChunk(b)
	require.NoError(t, err)
	require.Equal(t, int64(64), start)
	require.Equal(t, []byte{0xff, 0x0f}, bitmap)

	binary.LittleEndian.PutUint64(b, 3)

	_, _, err = decodeBitmapChunk(b)
	require.ErrorContains(t, err, "starting cluster")

	_, _, err = decodeBitmapChunk(b[:bitmapHeaderSize-1])
	require.Error(t, err)
}

func encodeRetrieval(startingVcn int64, runs ...[2]int64) []byte {
	b := binary.LittleEndian.AppendUint32(nil, uint32(len(runs)))
	b = binary.LittleEndian.AppendUint32(b, 0)
	b = binary.LittleEndian.AppendUint64(b, uint64(startingVcn))

	for _, r := range runs {
		b = binary.LittleEndian.AppendUint64(b, uint64(r[0]))
		b = binary.LittleEndian.AppendUint64(b, uint64(r[1]))
	}

	return b
}

func TestAppendRetrievalExtents(t *testing.T) {
	// clusters 0-9 at LCN 100, 10-14 sparse, 15-19 at LCN 50
	b := encodeRetrieval(0, [2]int64{10, 100}, [2]int64{15, unallocatedLcn}, [2]int64{20, 50})

	got, next, err := appendRetrievalExtents([]image.Extent{{Start: 1, Length: 1}}, b, 4096)
	require.NoError(t, err)
	require.Equal(t, int64(20), next)
	require.Equal(t, []image.Extent{{Start: 1, Length: 1}, {Start: 100 * 4096, Length: 10 * 4096}, {Start: 50 * 4096, Length: 5 * 4096}}, got)
}

func TestAppendRetrievalExtents_continuation(t *testing.T) {
	got, next, err := appendRetrievalExtents(nil, encodeRetrieval(20, [2]int64{25, 7}), 512)
	require.NoError(t, err)
	require.Equal(t, int64(25), next)
	require.Equal(t, []image.Extent{{Start: 7 * 512, Length: 5 * 512}}, got)
}

func TestAppendRetrievalExtents_invalid(t *testing.T) {
	_, _, err := appendRetrievalExtents(nil, encodeRetrieval(10, [2]int64{10, 1}), 4096)
	require.ErrorContains(t, err, "not increasing")

	_, _, err = appendRetrievalExtents(nil, encodeRetrieval(0, [2]int64{10, -2}), 4096)
	require.ErrorContains(t, err, "invalid cluster")

	b := encodeRetrieval(0, [2]int64{10, 1})

	_, _, err = appendRetrievalExtents(nil, b[:len(b)-1], 4096)
	require.ErrorContains(t, err, "too short")
}

func encodeDirEntry(name string, last bool) []byte {
	u := utf16.Encode([]rune(name))
	size := (dirInfoNameOff + 2*len(u) + 7) &^ 7

	e := make([]byte, size)
	if !last {
		binary.LittleEndian.PutUint32(e, uint32(size))
	}

	binary.LittleEndian.PutUint32(e[dirInfoNameLengthOff:], uint32(2*len(u)))

	for i, c := range u {
		binary.LittleEndian.PutUint16(e[dirInfoNameOff+2*i:], c)
	}

	return e
}

func TestDecodeDirectoryNames(t *testing.T) {
	diffArea := "{8a1c2e3f-4b5d-4a7e-9f00-112233445566}" + shadowStorageSuffix

	var b []byte

	b = append(b, encodeDirEntry(".", false)...)
	b = append(b, encodeDirEntry(diffArea, false)...)
	b = append(b, encodeDirEntry("tracking.log", true)...)
	b = append(b, 0xde, 0xad) // garbage after the last entry is ignored

	names, err := decodeDirectoryNames(b)
	require.NoError(t, err)
	require.Equal(t, []string{".", diffArea, "tracking.log"}, names)
}

func TestDecodeDirectoryNames_invalid(t *testing.T) {
	e := encodeDirEntry("abc", false)

	_, err := decodeDirectoryNames(e[:dirInfoNameOff-1])
	require.ErrorContains(t, err, "truncated")

	binary.LittleEndian.PutUint32(e[dirInfoNameLengthOff:], 3)

	_, err = decodeDirectoryNames(e)
	require.ErrorContains(t, err, "name length")

	e = encodeDirEntry("abc", false)
	binary.LittleEndian.PutUint32(e, 8)

	_, err = decodeDirectoryNames(e)
	require.ErrorContains(t, err, "next offset")
}

func TestIsShadowStorageFile(t *testing.T) {
	require.True(t, isShadowStorageFile("{8a1c2e3f-4b5d-4a7e-9f00-112233445566}"+shadowStorageSuffix))
	require.True(t, isShadowStorageFile("{8A1C2E3F-4B5D-4A7E-9F00-112233445566}{3808876B-C176-4E48-B7AE-04046E6CC752}"))
	require.False(t, isShadowStorageFile(shadowStorageSuffix))
	require.False(t, isShadowStorageFile("tracking.log"))
	require.False(t, isShadowStorageFile("IndexerVolumeGuid"))
}

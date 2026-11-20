package ntfsresize

import (
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"strconv"
	"testing"

	"github.com/pkg/errors"
	"github.com/stretchr/testify/require"
)

// testdata/ntfs32m.img.gz is a 32 MiB partition that Windows formatted with
// NTFS (4 KiB clusters) and filled with files up to cluster 5570.
const (
	fixtureLength   = 32 << 20
	fixtureLastUsed = 5570
)

// memDevice is a volume in memory that counts the bytes written to it.
type memDevice struct {
	b       []byte
	written int
}

func (m *memDevice) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 || off+int64(len(p)) > int64(len(m.b)) {
		return 0, errors.Errorf("read of %v bytes at %v past the end", len(p), off)
	}

	return copy(p, m.b[off:]), nil
}

func (m *memDevice) WriteAt(p []byte, off int64) (int, error) {
	if off < 0 || off+int64(len(p)) > int64(len(m.b)) {
		return 0, errors.Errorf("write of %v bytes at %v past the end", len(p), off)
	}

	m.written += len(p)

	return copy(m.b[off:], p), nil
}

func loadFixture(t *testing.T) []byte {
	t.Helper()

	f, err := os.Open("testdata/ntfs32m.img.gz")
	require.NoError(t, err)

	defer f.Close()

	r, err := gzip.NewReader(f)
	require.NoError(t, err)

	b, err := io.ReadAll(r)
	require.NoError(t, err)
	require.Len(t, b, fixtureLength)

	return b
}

func TestInspect(t *testing.T) {
	info, err := Inspect(&memDevice{b: loadFixture(t)})
	require.NoError(t, err)

	require.Equal(t, Boot{BytesPerSector: 512, ClusterSize: 4096, TotalSectors: 65535, MFTCluster: 2730, MFTMirrCluster: 2, RecordSize: 1024}, info.Boot)
	require.Equal(t, int64(8191), info.Boot.TotalClusters())
	require.True(t, info.BackupBootMatches)
	require.False(t, info.Dirty)
	require.Equal(t, int64(fixtureLastUsed), info.LastUsedCluster)

	// as Windows formats a volume: $Bitmap rounded to 8 bytes with the bits
	// past the last cluster set, $BadClus one sparse run over the volume
	require.Equal(t, StreamInfo{LastVCN: 0, Allocated: 4096, DataSize: 1024, Initialized: 1024, Runs: "1@2728"}, info.Bitmap)
	require.Equal(t, StreamInfo{LastVCN: 8190, Allocated: 8191 * 4096, DataSize: 8191 * 4096, Runs: "sparse:8191"}, info.BadClus)
	require.Equal(t, []byte{0, 0, 0, 0, 0, 0, 0, 0x80}, info.BitmapTail)
}

func TestShrink(t *testing.T) {
	orig := loadFixture(t)

	cases := []struct {
		name       string
		length     int64
		clusters   int64
		bitmapSize int64
		bitmapRuns string
		tail       []byte
	}{
		// Windows' own shrinks of such volumes changed exactly these fields
		{name: "by a quarter", length: 24 << 20, clusters: 6143, bitmapSize: 768, bitmapRuns: "1@2728", tail: []byte{0, 0, 0, 0, 0, 0, 0, 0x80}},
		{name: "to the last used cluster", length: (fixtureLastUsed+1)*4096 + 512, clusters: fixtureLastUsed + 1, bitmapSize: 704, bitmapRuns: "1@2728", tail: bytes.Repeat([]byte{0xff}, 8)}, // clusters 5568-5570 used, the rest past the end
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dev := &memDevice{b: bytes.Clone(orig)}

			require.NoError(t, Shrink(dev, tc.length))

			// the volume as the restored partition holds it
			info, err := Inspect(&memDevice{b: dev.b[:tc.length]})
			require.NoError(t, err)

			require.Equal(t, tc.length/512-1, info.Boot.TotalSectors)
			require.Equal(t, tc.clusters, info.Boot.TotalClusters())
			require.True(t, info.BackupBootMatches, "the backup boot sector is the last sector of the partition")
			require.True(t, info.Dirty, "the volume is marked for checking")
			require.Equal(t, int64(fixtureLastUsed), info.LastUsedCluster)

			require.Equal(t, StreamInfo{LastVCN: 0, Allocated: 4096, DataSize: tc.bitmapSize, Initialized: tc.bitmapSize, Runs: tc.bitmapRuns}, info.Bitmap)
			require.Equal(t, tc.tail, info.BitmapTail)
			require.Equal(t, StreamInfo{LastVCN: tc.clusters - 1, Allocated: tc.clusters * 4096, DataSize: tc.clusters * 4096, Runs: "sparse:" + strconv.FormatInt(tc.clusters, 10)}, info.BadClus)

			// the files' clusters did not change: only the boot sectors and
			// metadata records did
			for c := range tc.clusters {
				switch c {
				case 0, 2, 2728, 2730, 2731, 2732: // boot, $MFTMirr, $Bitmap, MFT records 0-11
					continue
				}

				if !bytes.Equal(orig[c*4096:(c+1)*4096], dev.b[c*4096:(c+1)*4096]) {
					t.Fatalf("cluster %v changed", c)
				}
			}
		})
	}
}

func TestShrink_inUse(t *testing.T) {
	orig := loadFixture(t)
	minLength := int64((fixtureLastUsed+1)*4096 + 512)

	for _, length := range []int64{minLength - 512, minLength - 4096, 16 << 20} {
		dev := &memDevice{b: bytes.Clone(orig)}

		err := Shrink(dev, length)
		require.ErrorIs(t, err, ErrInUse, length)
		require.Zero(t, dev.written, "nothing is written")
	}
}

func TestShrink_invalid(t *testing.T) {
	orig := loadFixture(t)

	dev := &memDevice{b: bytes.Clone(orig)}
	require.Error(t, Shrink(dev, 24<<20+100), "not a whole number of sectors")
	require.Error(t, Shrink(dev, 40<<20), "larger")

	notNTFS := &memDevice{b: make([]byte, 1<<20)}
	require.ErrorIs(t, Shrink(notNTFS, 512<<10), ErrNotNTFS)
	require.Zero(t, dev.written+notNTFS.written)
}

func TestMinPartitionLength(t *testing.T) {
	bitmap := []byte{0xff, 0x01, 0, 0} // clusters 0-8 used
	require.Equal(t, int64(9*4096+512), MinPartitionLength(bitmap, 32, 4096, 512))
	require.Equal(t, int64(512), MinPartitionLength(make([]byte, 4), 32, 4096, 512))

	// bits past the volume do not count
	require.Equal(t, int64(9*4096+512), MinPartitionLength([]byte{0xff, 0x01, 0, 0x80}, 31, 4096, 512))
}

func TestRuns(t *testing.T) {
	cases := [][]run{
		{{length: 8191, sparse: true}},
		{{length: 1, lcn: 2728}},
		{{length: 5, lcn: 49823}, {length: 3, lcn: 10}, {length: 200, sparse: true}, {length: 128, lcn: 1 << 40}},
		{{length: 0x80, lcn: 0x7f}, {length: 0x7f, lcn: 0x80}},
	}

	for _, runs := range cases {
		got, n, err := decodeRuns(encodeRuns(runs))
		require.NoError(t, err)
		require.Equal(t, runs, got)
		require.Len(t, encodeRuns(runs), n)
	}

	require.Equal(t, []run{{length: 5, lcn: 49823}, {length: 1, lcn: 10}}, truncateRuns(cases[2], 6))
	require.Equal(t, []run{{length: 2, lcn: 11}, {length: 128, lcn: 1 << 40}}, freedRuns(cases[2], 6))
}

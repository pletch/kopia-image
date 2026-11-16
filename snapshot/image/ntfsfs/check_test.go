package ntfsfs

import (
	"bytes"
	"encoding/binary"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/internal/repotesting"
)

func TestCheckCleanImage(t *testing.T) {
	ctx, env := repotesting.NewEnvironment(t, repotesting.FormatNotImportant)
	img := testImage(t)
	oid := writeImage(ctx, t, env.RepositoryWriter, img)

	v, err := Open(ctx, env.RepositoryWriter, oid, "p2-test", Options{})
	require.NoError(t, err)
	t.Cleanup(v.Close)

	var progress []int64

	r, err := v.Check(ctx, CheckOptions{Progress: func(done, total int64) {
		require.LessOrEqual(t, done, total)
		progress = append(progress, done)
	}})
	require.NoError(t, err)
	require.True(t, r.OK(), "%v", r.Problems)

	t.Logf("%+v", *r)

	require.Equal(t, int64(binary.LittleEndian.Uint16(img[0x0B:]))*int64(img[0x0D]), r.ClusterSize)
	require.Equal(t, int64(len(img))/r.ClusterSize-1, r.Clusters)
	require.Positive(t, r.Files)
	require.Positive(t, r.Directories)
	require.Positive(t, r.ReferencedClusters)
	require.LessOrEqual(t, r.ReferencedClusters, r.AllocatedClusters)
	require.NotEmpty(t, progress)
	require.Equal(t, r.MFTRecords, progress[len(progress)-1])
}

// testVolume locates structures of the test image for corrupting it.
type testVolume struct {
	img         []byte
	clusterSize int64
	recordSize  int64
	mftOffset   int64
}

func newTestVolume(t *testing.T) *testVolume {
	t.Helper()

	img := slices.Clone(testImage(t))
	sectorSize := int64(binary.LittleEndian.Uint16(img[0x0B:]))
	clusterSize := sectorSize * int64(img[0x0D])

	recordSize := int64(int8(img[0x40])) //nolint:gosec
	if recordSize < 0 {
		recordSize = 1 << -recordSize
	} else {
		recordSize *= clusterSize
	}

	return &testVolume{
		img:         img,
		clusterSize: clusterSize,
		recordSize:  recordSize,
		mftOffset:   int64(binary.LittleEndian.Uint64(img[0x30:])) * clusterSize, //nolint:gosec
	}
}

// record returns the raw bytes of an MFT record in the $MFT's first extent.
func (tv *testVolume) record(n int64) []byte {
	off := tv.mftOffset + n*tv.recordSize
	return tv.img[off : off+tv.recordSize]
}

// parsed returns a fixed-up copy of an MFT record.
func (tv *testVolume) parsed(t *testing.T, n int64) *mftRecord {
	t.Helper()

	rec, err := parseRecord(slices.Clone(tv.record(n)), n)
	require.NoError(t, err)

	return rec
}

// firstFile returns the first ordinary in-use file record.
func (tv *testVolume) firstFile(t *testing.T) int64 {
	t.Helper()

	for n := int64(firstOrdinaryRecord); n < 64; n++ {
		rec, err := parseRecord(slices.Clone(tv.record(n)), n)
		if err == nil && rec.flags == recordFlagInUse && rec.baseRef == 0 {
			return n
		}
	}

	t.Fatal("no file record found")

	return 0
}

func (tv *testVolume) check(t *testing.T) *CheckResult {
	t.Helper()

	r, err := CheckDevice(t.Context(), bytes.NewReader(tv.img), int64(len(tv.img)), CheckOptions{})
	require.NoError(t, err)

	t.Logf("problems: %v", r.Problems)

	return r
}

func requireProblem(t *testing.T, r *CheckResult, substr string) {
	t.Helper()

	require.False(t, r.OK())
	require.True(t, slices.ContainsFunc(r.Problems, func(p string) bool { return strings.Contains(p, substr) }),
		"no problem containing %q in %v", substr, r.Problems)
}

func TestCheckDetectsDamage(t *testing.T) {
	cases := []struct {
		name    string
		corrupt func(t *testing.T, tv *testVolume)
		problem string
	}{
		{
			name: "backup boot sector",
			corrupt: func(_ *testing.T, tv *testVolume) {
				tv.img[len(tv.img)-512+0x50]++
			},
			problem: "backup boot sector",
		},
		{
			name: "volume cut short",
			corrupt: func(_ *testing.T, tv *testVolume) {
				tv.img = tv.img[:len(tv.img)-4096]
			},
			problem: "does not fit in the image",
		},
		{
			name: "torn MFT record",
			corrupt: func(t *testing.T, tv *testVolume) {
				t.Helper()
				tv.record(tv.firstFile(t))[fixupStride-1]++
			},
			problem: "torn write",
		},
		{
			name: "zeroed MFT record",
			corrupt: func(t *testing.T, tv *testVolume) {
				t.Helper()
				clear(tv.record(tv.firstFile(t)))
			},
			problem: "is in use but blank",
		},
		{
			name: "misplaced MFT record",
			corrupt: func(t *testing.T, tv *testVolume) {
				t.Helper()
				n := tv.firstFile(t)
				binary.LittleEndian.PutUint32(tv.record(n)[0x2C:], uint32(n+1)) //nolint:gosec
			},
			problem: "holds record number",
		},
		{
			name: "file record marked free",
			corrupt: func(t *testing.T, tv *testVolume) {
				t.Helper()
				n := tv.firstFile(t)
				tv.record(n)[0x16] &^= recordFlagInUse
			},
			problem: "is marked in use in the $MFT bitmap but is free",
		},
		{
			name: "root directory freed",
			corrupt: func(_ *testing.T, tv *testVolume) {
				tv.record(rootMFTEntry)[0x16] &^= recordFlagInUse
			},
			problem: "the root directory (MFT record 5) is missing",
		},
		{
			name: "data clusters free in $Bitmap",
			corrupt: func(t *testing.T, tv *testVolume) {
				t.Helper()

				// free the $MFT's first cluster in $Bitmap
				a := findAttribute(tv.parsed(t, mftRecordBitmap), attrTypeData)
				require.NotNil(t, a)

				runs, err := decodeRuns(a.runList)
				require.NoError(t, err)

				mftLCN := tv.mftOffset / tv.clusterSize
				tv.img[runs[0].lcn*tv.clusterSize+mftLCN/8] &^= 1 << (mftLCN % 8)
			},
			problem: "free in $Bitmap, so imaged as zeros (first: LCN",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tv := newTestVolume(t)
			require.True(t, tv.check(t).OK())

			tc.corrupt(t, tv)
			requireProblem(t, tv.check(t), tc.problem)
		})
	}
}

func TestCheckNotNTFS(t *testing.T) {
	_, err := CheckDevice(t.Context(), bytes.NewReader(make([]byte, 1<<20)), 1<<20, CheckOptions{})
	require.ErrorIs(t, err, ErrNotNTFS)
}

func TestDecodeRuns(t *testing.T) {
	runs, err := decodeRuns([]byte{
		0x21, 0x10, 0x00, 0x01, // 16 clusters at LCN 256
		0x01, 0x08, // 8 sparse clusters
		0x11, 0x04, 0xF0, // 4 clusters at 256-16
		0x00,
	})
	require.NoError(t, err)
	require.Equal(t, []run{
		{lcn: 256, length: 16},
		{length: 8, sparse: true},
		{lcn: 240, length: 4},
	}, runs)

	for _, bad := range [][]byte{
		{0x21, 0x10, 0x00, 0x01},          // no end marker
		{0x21, 0x10},                      // truncated
		{0x20, 0x00, 0x01, 0x00},          // no length
		{0x11, 0x00, 0x01, 0x00},          // zero length
		{0x91, 0x01, 0, 0, 0, 0, 0, 0, 0}, // offset too long
	} {
		_, err := decodeRuns(bad)
		require.Error(t, err, "%x", bad)
	}
}

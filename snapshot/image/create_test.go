package image

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"math/rand/v2"
	"os"
	"strings"
	"testing"

	"github.com/pkg/errors"
	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/fs"
	"github.com/kopia/kopia/internal/repotesting"
	"github.com/kopia/kopia/snapshot"
	"github.com/kopia/kopia/snapshot/snapshotfs"
)

func randomBytes(seed uint64, n int) []byte {
	r := rand.New(rand.NewPCG(seed, seed)) //nolint:gosec
	b := make([]byte, n)

	for i := range b {
		b[i] = byte(r.Uint32())
	}

	return b
}

// testVolumeData returns 3 chunks of data, a zero chunk and a partial chunk.
func testVolumeData() []byte {
	d := randomBytes(1, 4*DefaultChunkSize+12345)
	clear(d[2*DefaultChunkSize : 3*DefaultChunkSize])

	return d
}

func bytesSource(data []byte) func() (io.ReadCloser, error) {
	return func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(data)), nil
	}
}

func testSource(t *testing.T) snapshot.SourceInfo {
	t.Helper()

	p, err := DiskSourcePath(testDiskGUID)
	require.NoError(t, err)

	return snapshot.SourceInfo{Host: "desktop", UserName: "tim", Path: p}
}

func testCreateOptions(t *testing.T, ntfsData, rawData []byte) *CreateOptions {
	t.Helper()

	return &CreateOptions{
		Source:      testSource(t),
		Description: "test image",
		Layout:      testLayout(),
		Volumes: []VolumeSource{
			{
				PartitionNumber: 3, Label: "C", Kind: VolumeKindNTFS, Size: int64(len(ntfsData)), Open: bytesSource(ntfsData),
				ClusterSize: 4096, SerialNumber: "8a1c2e3f4b5d6a7e", Bitmap: []byte{0xff, 0x0f},
				UnstableExtents: []Extent{{Start: 4096, Length: 8192}},
			},
			{PartitionNumber: 1, Label: "esp", Kind: VolumeKindRaw, Size: int64(len(rawData)), Open: bytesSource(rawData)},
		},
		Compressor:         "zstd-fastest",
		MetadataCompressor: "zstd-fastest",
		AsyncWrites:        4,
		Tags:               map[string]string{"tag:host-kind": "test"},
	}
}

func readObject(ctx context.Context, t *testing.T, e fs.Entry) []byte {
	t.Helper()

	f, ok := e.(fs.File)
	require.True(t, ok, "%v is not a file", e.Name())

	r, err := f.Open(ctx)
	require.NoError(t, err)

	defer r.Close()

	b, err := io.ReadAll(r)
	require.NoError(t, err)

	return b
}

func TestCreate(t *testing.T) {
	ctx, env := repotesting.NewEnvironment(t, repotesting.FormatNotImportant)

	ntfsData := testVolumeData()
	rawData := randomBytes(2, 3<<20)

	man, md, err := Create(ctx, env.RepositoryWriter, testCreateOptions(t, ntfsData, rawData))
	require.NoError(t, err)

	require.Equal(t, TagValue, man.Tags[TagKey])
	require.Equal(t, "test", man.Tags["tag:host-kind"])
	require.NotNil(t, md.UploadedBytes)
	require.Positive(t, *md.UploadedBytes)

	require.Len(t, md.Volumes, 2)
	require.Equal(t, "p1-esp.img", md.Volumes[0].EntryName)
	require.Equal(t, "p3-C.img", md.Volumes[1].EntryName)
	require.Equal(t, VolumeStats{BytesRead: int64(len(ntfsData)), ChunksTotal: 5, ZeroChunks: 1}, md.Volumes[1].Stats)

	snaps, err := snapshot.ListSnapshots(ctx, env.Repository, testSource(t))
	require.NoError(t, err)
	require.Len(t, snaps, 1)

	root, err := snapshotfs.SnapshotRoot(env.Repository, snaps[0])
	require.NoError(t, err)

	dir, ok := root.(fs.Directory)
	require.True(t, ok)

	entries, err := fs.GetAllEntries(ctx, dir)
	require.NoError(t, err)

	byName := map[string]fs.Entry{}
	for _, e := range entries {
		byName[e.Name()] = e
	}

	require.Len(t, byName, 6)
	require.Equal(t, ntfsData, readObject(ctx, t, byName["p3-C.img"]))
	require.Equal(t, rawData, readObject(ctx, t, byName["p1-esp.img"]))
	require.Equal(t, []byte{0xff, 0x0f}, readObject(ctx, t, byName["p3-C.bitmap"]))

	var gotMD Metadata

	require.NoError(t, json.Unmarshal(readObject(ctx, t, byName[MetadataFileName]), &gotMD))
	require.Equal(t, md.Volumes, gotMD.Volumes)

	var gotLayout Layout

	require.NoError(t, json.Unmarshal(readObject(ctx, t, byName[LayoutFileName]), &gotLayout))
	require.Equal(t, testLayout(), &gotLayout)

	script := string(readObject(ctx, t, byName[RestoreScriptFileName]))
	require.Contains(t, script, "kopia show '"+md.Volumes[1].ObjectID.String()+"'")
	require.Equal(t, os.FileMode(executablePermissions), entries[slicesIndex(entries, RestoreScriptFileName)].Mode().Perm())

	// the image object can be streamed by "kopia show" as restore.sh does
	r, err := env.Repository.OpenObject(ctx, md.Volumes[1].ObjectID)
	require.NoError(t, err)

	streamed, err := io.ReadAll(r)
	require.NoError(t, err)
	require.NoError(t, r.Close())
	require.Equal(t, ntfsData, streamed)
}

func slicesIndex(entries []fs.Entry, name string) int {
	for i, e := range entries {
		if e.Name() == name {
			return i
		}
	}

	return -1
}

func TestCreate_secondRunUploadsOnlyChanges(t *testing.T) {
	ctx, env := repotesting.NewEnvironment(t, repotesting.FormatNotImportant)

	ntfsData := testVolumeData()
	rawData := randomBytes(2, 3<<20)

	_, first, err := Create(ctx, env.RepositoryWriter, testCreateOptions(t, ntfsData, rawData))
	require.NoError(t, err)

	// change one byte in the first chunk
	changed := bytes.Clone(ntfsData)
	changed[100] ^= 0xff

	_, second, err := Create(ctx, env.RepositoryWriter, testCreateOptions(t, changed, rawData))
	require.NoError(t, err)

	t.Logf("uploaded: first %v, second %v", *first.UploadedBytes, *second.UploadedBytes)
	require.Less(t, *second.UploadedBytes, *first.UploadedBytes/3)
	require.NotEqual(t, first.Volumes[1].ObjectID, second.Volumes[1].ObjectID)
	require.Equal(t, first.Volumes[0].ObjectID, second.Volumes[0].ObjectID)

	snaps, err := snapshot.ListSnapshots(ctx, env.Repository, testSource(t))
	require.NoError(t, err)
	require.Len(t, snaps, 2)
}

// failingReader returns its data and then an error instead of the rest.
type failingReader struct {
	r   io.Reader
	err error
}

func (f *failingReader) Read(p []byte) (int, error) {
	n, err := f.r.Read(p)
	if errors.Is(err, io.EOF) {
		return n, f.err
	}

	return n, err
}

func (f *failingReader) Close() error { return nil }

func TestCreate_retryAfterFailureReusesChunks(t *testing.T) {
	ctx, env := repotesting.NewEnvironment(t, repotesting.FormatNotImportant)

	ntfsData := testVolumeData()
	rawData := randomBytes(2, 1<<20)
	errNetwork := errors.New("connection reset")

	// the volume fails in its last chunk, after 4 chunks were written
	failing := testCreateOptions(t, ntfsData, rawData)
	failing.Volumes[0].Open = func() (io.ReadCloser, error) {
		return &failingReader{r: bytes.NewReader(ntfsData[:4*DefaultChunkSize+100]), err: errNetwork}, nil
	}

	_, _, err := Create(ctx, env.RepositoryWriter, failing)
	require.ErrorIs(t, err, errNetwork)

	_, md, err := Create(ctx, env.RepositoryWriter, testCreateOptions(t, ntfsData, rawData))
	require.NoError(t, err)

	t.Logf("uploaded by the retry: %v", *md.UploadedBytes)
	require.Less(t, *md.UploadedBytes, int64(DefaultChunkSize), "the retry should upload only the last partial chunk and metadata")
}

func TestCreate_periodicFlush(t *testing.T) {
	ctx, env := repotesting.NewEnvironment(t, repotesting.FormatNotImportant)

	opt := testCreateOptions(t, testVolumeData(), randomBytes(2, 1<<20))
	opt.FlushInterval = 1 // flush as often as possible to exercise concurrent flushes

	_, _, err := Create(ctx, env.RepositoryWriter, opt)
	require.NoError(t, err)
}

func TestCreate_failureSavesNoSnapshot(t *testing.T) {
	errBoom := errors.New("device error")

	cases := []struct {
		name   string
		mutate func(o *CreateOptions)
		want   error
	}{
		{"short volume", func(o *CreateOptions) { o.Volumes[0].Size++ }, io.ErrUnexpectedEOF},
		{"open error", func(o *CreateOptions) {
			o.Volumes[0].Open = func() (io.ReadCloser, error) { return nil, errBoom }
		}, errBoom},
		{"unstable raw volume", func(o *CreateOptions) {
			n := 0
			o.Volumes[1].Open = func() (io.ReadCloser, error) {
				n++
				return io.NopCloser(strings.NewReader(strings.Repeat(string(rune('a'+n%26)), 1000))), nil
			}
			o.Volumes[1].Size = 1000
		}, ErrUnstableSource},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, env := repotesting.NewEnvironment(t, repotesting.FormatNotImportant)

			opt := testCreateOptions(t, testVolumeData(), randomBytes(2, 1<<20))
			tc.mutate(opt)

			_, _, err := Create(ctx, env.RepositoryWriter, opt)
			require.ErrorIs(t, err, tc.want)

			snaps, err := snapshot.ListSnapshots(ctx, env.Repository, testSource(t))
			require.NoError(t, err)
			require.Empty(t, snaps)
		})
	}
}

func TestCreate_invalidOptions(t *testing.T) {
	ctx, env := repotesting.NewEnvironment(t, repotesting.FormatNotImportant)

	opt := testCreateOptions(t, testVolumeData(), randomBytes(2, 1<<20))
	opt.Source.Path = `C:\`

	_, _, err := Create(ctx, env.RepositoryWriter, opt)
	require.ErrorContains(t, err, "invalid source")

	opt = testCreateOptions(t, testVolumeData(), randomBytes(2, 1<<20))
	opt.Volumes = nil

	_, _, err = Create(ctx, env.RepositoryWriter, opt)
	require.ErrorContains(t, err, "no volumes")
}

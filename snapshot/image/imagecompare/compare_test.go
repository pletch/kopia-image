package imagecompare

import (
	"context"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/internal/repotesting"
	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/object"
	"github.com/kopia/kopia/snapshot/image"
	"github.com/kopia/kopia/snapshot/image/imagerestore"
	"github.com/kopia/kopia/snapshot/image/ntfsfs"
)

const (
	mib   = 1 << 20
	chunk = 4 << 20 // images of kopia v0.23.1-wails.39 and older
)

func writeObject(ctx context.Context, t *testing.T, rep repo.RepositoryWriter, data []byte) object.ID {
	t.Helper()

	w := rep.NewObjectWriter(ctx, object.WriterOptions{Splitter: "FIXED-4M"})
	defer w.Close()

	_, err := w.Write(data)
	require.NoError(t, err)

	oid, err := w.Result()
	require.NoError(t, err)

	return oid
}

func snapshotOf(oid object.ID, size int64, v *image.Volume) *imagerestore.Snapshot {
	v.EntryName = "p1-test.img"
	v.PartitionNumber = 1
	v.ObjectID = oid
	v.Size = size

	return &imagerestore.Snapshot{Metadata: &image.Metadata{ChunkSize: chunk, Volumes: []image.Volume{*v}}}
}

func randomBytes(seed uint64, n int) []byte {
	r := rand.New(rand.NewPCG(seed, seed)) //nolint:gosec
	b := make([]byte, n)

	for i := range b {
		b[i] = byte(r.Uint32())
	}

	return b
}

// rawPair writes an 8-chunk volume and a copy with known changes.
func rawPair(ctx context.Context, t *testing.T, rep repo.RepositoryWriter) (older, newer *imagerestore.Snapshot) {
	t.Helper()

	old := randomBytes(1, 8*chunk)
	cur := slices.Clone(old)

	// chunk 1: one 4 KiB unit
	cur[chunk+100<<10] ^= 0xFF
	// chunk 2: first MiB zeroed, as when files are deleted
	clear(cur[2*chunk : 2*chunk+mib])
	// chunk 3: second half rewritten
	copy(cur[3*chunk+2*mib:4*chunk], randomBytes(2, 2*mib))
	// chunk 4: second half now holds a copy of the first half
	copy(cur[4*chunk+2*mib:5*chunk], old[4*chunk:4*chunk+2*mib])
	// chunk 5: holds what chunk 6 holds
	copy(cur[5*chunk:6*chunk], old[6*chunk:7*chunk])

	older = snapshotOf(writeObject(ctx, t, rep, old), int64(len(old)), &image.Volume{Kind: image.VolumeKindRaw})
	newer = snapshotOf(writeObject(ctx, t, rep, cur), int64(len(cur)), &image.Volume{Kind: image.VolumeKindRaw})

	return older, newer
}

func TestCompare_raw(t *testing.T) {
	ctx, env := repotesting.NewEnvironment(t, repotesting.FormatNotImportant)
	older, newer := rawPair(ctx, t, env.RepositoryWriter)

	var stages []string

	res, err := Compare(ctx, env.RepositoryWriter, older, newer, Options{
		Seed:  1,
		Files: true,
		Progress: func(_, stage string, done, total int64) {
			require.LessOrEqual(t, done, total)

			stages = append(stages, stage)
		},
	})
	require.NoError(t, err)
	require.Empty(t, res.Unmatched)
	require.Len(t, res.Volumes, 1)

	v := res.Volumes[0]
	require.Equal(t, 8, v.Chunks)
	require.Equal(t, 3, v.UnchangedChunks) // 0, 6, 7
	require.Equal(t, 1, v.MovedChunks)     // 5
	require.Equal(t, 4, v.NewChunks)       // 1-4
	require.EqualValues(t, 4*chunk, v.NewChunkBytes)
	require.Equal(t, 4, v.SampledChunks)
	require.EqualValues(t, 4*chunk, v.SampledBytes)
	require.EqualValues(t, 4<<10+mib+2*mib+2*mib, v.ChangedBytes)

	got := map[int64]int64{}
	for _, b := range v.Blocks {
		require.Equal(t, b.SampleNewBytes, b.NewBytes, "the whole sample: nothing to extrapolate")
		require.Equal(t, 8*chunk/b.Size, b.ListEntries)
		got[b.Size] = b.NewBytes
	}

	require.Equal(t, map[int64]int64{
		// every new chunk, zeros and copies included
		4 * mib: 4 * chunk,
		// chunk 1 and 2: the changed half; chunk 3: the rewritten half;
		// chunk 4: the copy is a block the old image has
		2 * mib: 2*mib + 2*mib + 2*mib,
		// chunk 1: one block; chunk 2: zeros; chunk 3: two blocks
		1 * mib:   mib + 2*mib,
		512 << 10: 512<<10 + 2*mib,
		256 << 10: 256<<10 + 2*mib,
		128 << 10: 128<<10 + 2*mib,
		64 << 10:  64<<10 + 2*mib,
	}, got)

	require.Equal(t, "not an NTFS volume", v.FilesError)
	require.Contains(t, stages, StageChunks)
}

func TestCompare_sample(t *testing.T) {
	ctx, env := repotesting.NewEnvironment(t, repotesting.FormatNotImportant)
	older, newer := rawPair(ctx, t, env.RepositoryWriter)

	a, err := Compare(ctx, env.RepositoryWriter, older, newer, Options{Sample: 2, Seed: 7})
	require.NoError(t, err)

	b, err := Compare(ctx, env.RepositoryWriter, older, newer, Options{Sample: 2, Seed: 7})
	require.NoError(t, err)

	require.Equal(t, a, b, "the same seed picks the same sample")

	v := a.Volumes[0]
	require.Equal(t, 2, v.SampledChunks)
	require.EqualValues(t, 2*chunk, v.SampledBytes)

	for _, e := range v.Blocks {
		require.Equal(t, e.SampleNewBytes*2, e.NewBytes, "half the new chunks were sampled")
	}
}

func TestCompare_mismatched(t *testing.T) {
	ctx, env := repotesting.NewEnvironment(t, repotesting.FormatNotImportant)
	older, newer := rawPair(ctx, t, env.RepositoryWriter)

	newer.Metadata.Volumes[0].PartitionNumber = 2

	res, err := Compare(ctx, env.RepositoryWriter, older, newer, Options{})
	require.NoError(t, err)
	require.Empty(t, res.Volumes)
	require.Equal(t, []string{"p1-test.img (new image only)", "p1-test.img (old image only)"}, res.Unmatched)

	newer.Metadata.ChunkSize = mib
	_, err = Compare(ctx, env.RepositoryWriter, older, newer, Options{})
	require.ErrorContains(t, err, "different chunk sizes")

	newer.Metadata.ChunkSize = chunk
	_, err = Compare(ctx, env.RepositoryWriter, older, newer, Options{BlockSizes: []int64{1000}})
	require.ErrorContains(t, err, "not a multiple")
}

func ntfsTestImage(t *testing.T) []byte {
	t.Helper()

	out, err := exec.CommandContext(t.Context(), "go", "list", "-m", "-f", "{{.Dir}}", "www.velocidex.com/golang/go-ntfs").Output()
	if err != nil {
		t.Skipf("go-ntfs module not found: %v", err)
	}

	b, err := os.ReadFile(filepath.Join(strings.TrimSpace(string(out)), "parser", "test_data", "test.ntfs.dd"))
	require.NoError(t, err)

	return b
}

func TestCompare_files(t *testing.T) {
	ctx, env := repotesting.NewEnvironment(t, repotesting.FormatNotImportant)
	rep := env.RepositoryWriter

	old := ntfsTestImage(t)
	clusterSize := int64(old[0x0B]) | int64(old[0x0C])<<8
	clusterSize *= int64(old[0x0D])

	oldID := writeObject(ctx, t, rep, old)

	// find a cluster of ones.bin and a free one
	vol, err := ntfsfs.Open(ctx, rep, oldID, "p1-test", ntfsfs.Options{})
	require.NoError(t, err)

	var all []int64
	for lcn := range int64(len(old))/clusterSize - 1 {
		all = append(all, lcn)
	}

	uses, err := vol.ClusterUses(ctx, all, ntfsfs.ClusterUseOptions{})
	require.NoError(t, err)
	vol.Close()

	onesLCN, freeLCN := int64(-1), int64(-1)

	for _, lcn := range all {
		switch u := uses[lcn]; {
		case u.Path == "ones.bin" && onesLCN < 0:
			onesLCN = lcn
		case u.Free && freeLCN < 0 && lcn > 0:
			freeLCN = lcn
		}
	}

	require.GreaterOrEqual(t, onesLCN, int64(0))
	require.GreaterOrEqual(t, freeLCN, int64(0))

	cur := slices.Clone(old)
	cur[onesLCN*clusterSize] ^= 0xFF
	cur[onesLCN*clusterSize+clusterSize/2] ^= 0xFF // same unit, counted once
	cur[freeLCN*clusterSize] ^= 0xFF

	ntfsVol := &image.Volume{Kind: image.VolumeKindNTFS, ClusterSize: clusterSize}
	older := snapshotOf(oldID, int64(len(old)), ntfsVol)
	newer := snapshotOf(writeObject(ctx, t, rep, cur), int64(len(cur)), ntfsVol)

	res, err := Compare(ctx, rep, older, newer, Options{Files: true, UnitSize: clusterSize, BlockSizes: []int64{chunk, clusterSize}})
	require.NoError(t, err)

	v := res.Volumes[0]
	require.Empty(t, v.FilesError)
	require.Equal(t, 2*clusterSize, v.ChangedBytes)

	groups := map[string]GroupEstimate{}
	for _, g := range v.Groups {
		groups[g.Path] = g
	}

	require.Len(t, groups, 2, "%+v", v.Groups)
	require.Equal(t, clusterSize, groups["ones.bin"].ChangedBytes)
	require.Equal(t, clusterSize, groups[GroupFreeSpace].ChangedBytes)

	// the new chunks' cost is shared among the groups that changed them
	var cost int64
	for _, g := range v.Groups {
		cost += g.ChunkCost
	}

	require.InDelta(t, v.NewChunkBytes, cost, 2)
}

func TestGroupOf(t *testing.T) {
	require.Equal(t, `Users\me\AppData\Local\go-build`, groupOf(`Users\me\AppData\Local\go-build\ab\abc-d`, 5))
	require.Equal(t, `ones.bin`, groupOf(`ones.bin`, 5))
	require.Equal(t, `$Extend`, groupOf(`$Extend\$UsnJrnl`, 1))
}

func TestPickSample(t *testing.T) {
	c := []int{3, 5, 8, 13, 21}

	require.Equal(t, c, pickSample(c, 10, 1))

	s := pickSample(c, 3, 1)
	require.Len(t, s, 3)
	require.True(t, slices.IsSorted(s))

	for _, x := range s {
		require.Contains(t, c, x)
	}

	require.Equal(t, s, pickSample(c, 3, 1))
}

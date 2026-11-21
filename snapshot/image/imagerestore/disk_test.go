package imagerestore

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/internal/repotesting"
	"github.com/kopia/kopia/snapshot"
	"github.com/kopia/kopia/snapshot/image"
	"github.com/kopia/kopia/snapshot/image/gpt"
)

func diskLayout() *image.Layout {
	return &image.Layout{
		DiskGUID:   "0f6ab3f2-1c3d-4e5f-8a9b-0c1d2e3f4a5b",
		SectorSize: 512,
		DiskSize:   96 << 20,
		Partitions: []image.Partition{
			{Number: 1, TypeGUID: image.PartitionTypeEFISystem, UniqueGUID: "11111111-2222-4333-8444-555555555551", Offset: 1 << 20, Length: 8 << 20, Name: "EFI system partition", Attributes: 1 << 63},
			{Number: 2, TypeGUID: image.PartitionTypeMSR, UniqueGUID: "11111111-2222-4333-8444-555555555552", Offset: 9 << 20, Length: 16 << 20, Name: "Microsoft reserved partition"},
			{Number: 3, TypeGUID: image.PartitionTypeBasicData, UniqueGUID: "11111111-2222-4333-8444-555555555553", Offset: 25 << 20, Length: 41 << 20, Name: "Basic data partition"},
		},
	}
}

func bytesSource(b []byte) func() (io.ReadCloser, error) {
	return func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(b)), nil }
}

// createImageSnapshot images an ESP and an NTFS volume and returns the
// snapshot, the volume contents by partition number and the repository.
func createImageSnapshot(t *testing.T) (context.Context, *repotesting.Environment, *Snapshot, map[int][]byte) {
	t.Helper()

	ctx, env := repotesting.NewEnvironment(t, repotesting.FormatNotImportant)

	contents := map[int][]byte{1: randomBytes(7, 8<<20), 3: testImage()}
	layout := diskLayout()

	path, err := image.DiskSourcePath(layout.DiskGUID)
	require.NoError(t, err)

	man, _, err := image.Create(ctx, env.RepositoryWriter, &image.CreateOptions{
		Source: snapshot.SourceInfo{Host: "h", UserName: "u", Path: path},
		Layout: layout,
		Volumes: []image.VolumeSource{
			{PartitionNumber: 1, Label: "esp", Kind: image.VolumeKindRaw, Size: int64(len(contents[1])), Open: bytesSource(contents[1])},
			{PartitionNumber: 3, Label: "C", Kind: image.VolumeKindNTFS, Size: int64(len(contents[3])), Open: bytesSource(contents[3]), ClusterSize: 4096},
		},
		Compressor: "zstd-fastest",
		ChunkSize:  chunk, // as testImage is made of
	})
	require.NoError(t, err)

	s, err := LoadSnapshot(ctx, env.Repository, man)
	require.NoError(t, err)

	return ctx, env, s, contents
}

func readRange(t *testing.T, path string, off, length int64) []byte {
	t.Helper()

	f, err := os.Open(path)
	require.NoError(t, err)

	defer f.Close()

	b := make([]byte, length)
	_, err = f.ReadAt(b, off)
	require.NoError(t, err)

	return b
}

func verifyRestoredDisk(t *testing.T, path string, s *Snapshot, contents map[int][]byte) {
	t.Helper()

	f, err := os.Open(path)
	require.NoError(t, err)

	table, err := gpt.Read(f, 512)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	require.Equal(t, s.Layout.DiskGUID, table.DiskGUID)
	require.Equal(t, s.Layout.Partitions, table.Partitions)

	for n, want := range contents {
		p, _ := s.Layout.Partition(n)
		require.True(t, bytes.Equal(want, readRange(t, path, p.Offset, int64(len(want)))), "partition %v", n)
	}

	msr, _ := s.Layout.Partition(2)
	require.Equal(t, make([]byte, msr.Length), readRange(t, path, msr.Offset, msr.Length), "the MSR is zeroed")
}

func TestRestoreDisk_newFile(t *testing.T) {
	ctx, env, s, contents := createImageSnapshot(t)

	path := filepath.Join(t.TempDir(), "disk.img")

	target, err := OpenTarget(path, s.Layout.DiskSize, s.Layout.SectorSize)
	require.NoError(t, err)
	require.True(t, target.IsZeroed())
	require.NoError(t, s.CheckTarget(target, false))

	has, err := HasPartitionTable(target)
	require.NoError(t, err)
	require.False(t, has)

	results, err := RestoreDisk(ctx, env.Repository, s, target, DiskOptions{Volumes: s.Metadata.Volumes, Parallel: 4})
	require.NoError(t, err)
	require.Len(t, results, 2)
	require.Equal(t, int64(4*chunk), results[1].Stats.ZeroBytes)

	has, err = HasPartitionTable(target)
	require.NoError(t, err)
	require.True(t, has)
	require.NoError(t, target.Close())

	verifyRestoredDisk(t, path, s, contents)
}

func TestRestoreDisk_overwritesDirtyFileAndPartialRestore(t *testing.T) {
	ctx, env, s, contents := createImageSnapshot(t)

	// a larger disk full of garbage: zero chunks and the MSR must be zeroed
	path := filepath.Join(t.TempDir(), "disk.img")
	require.NoError(t, os.WriteFile(path, bytes.Repeat([]byte{0xee}, 128<<20), 0o600))

	target, err := OpenTarget(path, 0, 512)
	require.NoError(t, err)
	require.False(t, target.IsZeroed())

	_, err = RestoreDisk(ctx, env.Repository, s, target, DiskOptions{Volumes: s.Metadata.Volumes})
	require.NoError(t, err)
	require.NoError(t, target.Close())

	verifyRestoredDisk(t, path, s, contents)

	// damage the NTFS partition and restore only that volume
	p3, _ := s.Layout.Partition(3)
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	require.NoError(t, err)
	_, err = f.WriteAt(bytes.Repeat([]byte{1}, 1<<20), p3.Offset+5<<20)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	only, err := s.SelectVolumes([]string{"p3-C"})
	require.NoError(t, err)
	require.Len(t, only, 1)

	target, err = OpenTarget(path, 0, 512)
	require.NoError(t, err)

	results, err := RestoreDisk(ctx, env.Repository, s, target, DiskOptions{Volumes: only})
	require.NoError(t, err)
	require.Len(t, results, 1)
	require.NoError(t, target.Close())

	verifyRestoredDisk(t, path, s, contents)
}

func TestRestoreDisk_partialRestoreNeedsMatchingTable(t *testing.T) {
	ctx, env, s, _ := createImageSnapshot(t)

	path := filepath.Join(t.TempDir(), "blank.img")

	target, err := OpenTarget(path, s.Layout.DiskSize, 512)
	require.NoError(t, err)

	defer target.Close()

	only, err := s.SelectVolumes([]string{"3"})
	require.NoError(t, err)

	_, err = RestoreDisk(ctx, env.Repository, s, target, DiskOptions{Volumes: only})
	require.ErrorIs(t, err, gpt.ErrNoGPT)
}

func loadTestSnapshot(t *testing.T) *Snapshot {
	t.Helper()

	_, _, s, _ := createImageSnapshot(t) //nolint:dogsled

	return s
}

func TestCheckTarget(t *testing.T) {
	s := loadTestSnapshot(t)
	dir := t.TempDir()

	small, err := OpenTarget(filepath.Join(dir, "small.img"), 32<<20, 512)
	require.NoError(t, err)

	defer small.Close()

	require.ErrorContains(t, s.CheckTarget(small, false), "needs at least")

	big4k, err := OpenTarget(filepath.Join(dir, "4k.img"), 128<<20, 4096)
	require.NoError(t, err)

	defer big4k.Close()

	require.ErrorContains(t, s.CheckTarget(big4k, false), "4096-byte sectors")
}

func TestSelectVolumes(t *testing.T) {
	s := loadTestSnapshot(t)

	all, err := s.SelectVolumes(nil)
	require.NoError(t, err)
	require.Len(t, all, 2)

	for _, names := range [][]string{{"p1-esp.img"}, {"p1-esp"}, {"1"}, {"1", "p1-esp"}} {
		got, err := s.SelectVolumes(names)
		require.NoError(t, err)
		require.Len(t, got, 1, "%v", names)
		require.Equal(t, 1, got[0].PartitionNumber)
	}

	_, err = s.SelectVolumes([]string{"p2-msr"})
	require.ErrorContains(t, err, "no volume")
}

// failingTarget fails writes past a limit, like a disk that disconnects.
type failingTarget struct {
	Target

	limit int64
}

func (f *failingTarget) WriteAt(p []byte, off int64) (int, error) {
	if off+int64(len(p)) > f.limit {
		return 0, io.ErrShortWrite
	}

	return f.Target.WriteAt(p, off) //nolint:wrapcheck
}

func TestRestoreDisk_interruptedLeavesNoPartitionTable(t *testing.T) {
	ctx, env, s, _ := createImageSnapshot(t)

	// a disk that had the image's partition table
	path := filepath.Join(t.TempDir(), "disk.img")

	target, err := OpenTarget(path, s.Layout.DiskSize, 512)
	require.NoError(t, err)

	_, err = RestoreDisk(ctx, env.Repository, s, target, DiskOptions{Volumes: s.Metadata.Volumes})
	require.NoError(t, err)

	// a second restore fails partway through the NTFS volume
	p3, _ := s.Layout.Partition(3)

	_, err = RestoreDisk(ctx, env.Repository, s, &failingTarget{Target: target, limit: p3.Offset + 8<<20}, DiskOptions{Volumes: s.Metadata.Volumes})
	require.ErrorIs(t, err, io.ErrShortWrite)

	_, err = gpt.Read(target, 512)
	require.ErrorIs(t, err, gpt.ErrNoGPT, "an interrupted restore must not leave a partition table")
	require.NoError(t, target.Close())
}

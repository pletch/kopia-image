package ntfsfs

import (
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/fs"
	"github.com/kopia/kopia/internal/repotesting"
	"github.com/kopia/kopia/snapshot"
	"github.com/kopia/kopia/snapshot/image"
	"github.com/kopia/kopia/snapshot/snapshotfs"
)

// createTestImageSnapshot stores go-ntfs's test image as the NTFS partition 2
// of an image snapshot, next to a raw ESP, and returns the snapshot root.
func createTestImageSnapshot(t *testing.T) (context.Context, *repotesting.Environment, fs.Directory) {
	t.Helper()

	ctx, env := repotesting.NewEnvironment(t, repotesting.FormatNotImportant)

	ntfsData := testImage(t)
	esp := bytes.Repeat([]byte{1}, 1<<20)

	open := func(b []byte) func() (io.ReadCloser, error) {
		return func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(b)), nil }
	}

	const diskGUID = "0f6ab3f2-1c3d-4e5f-8a9b-0c1d2e3f4a5b"

	path, err := image.DiskSourcePath(diskGUID)
	require.NoError(t, err)

	man, _, err := image.Create(ctx, env.RepositoryWriter, &image.CreateOptions{
		Source: snapshot.SourceInfo{Host: "h", UserName: "u", Path: path},
		Layout: &image.Layout{
			DiskGUID:   diskGUID,
			SectorSize: 512,
			DiskSize:   1 << 30,
			Partitions: []image.Partition{
				{Number: 1, TypeGUID: image.PartitionTypeEFISystem, UniqueGUID: "11111111-2222-4333-8444-555555555551", Offset: 1 << 20, Length: 8 << 20, Name: "EFI system partition"},
				{Number: 2, TypeGUID: image.PartitionTypeBasicData, UniqueGUID: "11111111-2222-4333-8444-555555555552", Offset: 9 << 20, Length: 16 << 20, Name: "Basic data partition"},
			},
		},
		Volumes: []image.VolumeSource{
			{PartitionNumber: 1, Label: "esp", Kind: image.VolumeKindRaw, Size: int64(len(esp)), Open: open(esp)},
			{PartitionNumber: 2, Label: "C", Kind: image.VolumeKindNTFS, Size: int64(len(ntfsData)), Open: open(ntfsData)},
		},
	})
	require.NoError(t, err)

	root, err := snapshotfs.SnapshotRoot(env.Repository, man)
	require.NoError(t, err)

	dir, ok := root.(fs.Directory)
	require.True(t, ok)

	return ctx, env, dir
}

func TestWithBrowsableVolumes(t *testing.T) {
	ctx, env, root := createTestImageSnapshot(t)

	// mounts are created in requests whose context ends; the volume must keep working
	reqCtx, cancel := context.WithCancel(ctx)
	wrapped := WithBrowsableVolumes(reqCtx, env.Repository, root)

	cancel()

	got := names(ctx, t, wrapped)
	require.Contains(t, got, "p2-C", "a folder for the NTFS volume")
	require.Contains(t, got, "p2-C.img", "the image itself stays")
	require.NotContains(t, got, "p1-esp", "raw volumes get no folder")

	vol, err := wrapped.Child(ctx, "p2-C")
	require.NoError(t, err)
	require.True(t, vol.IsDir())

	volDir, ok := vol.(fs.Directory)
	require.True(t, ok)

	hello := lookup(ctx, t, volDir, "Folder A/Folder B/Hello world text document.txt")
	require.Equal(t, "Hello world!", string(readFile(ctx, t, hello)))

	// the snapshot's own entries still resolve
	_, err = wrapped.Child(ctx, image.LayoutFileName)
	require.NoError(t, err)
}

func TestWithBrowsableVolumes_otherDirectoriesUnchanged(t *testing.T) {
	ctx, env, root := createTestImageSnapshot(t)

	// a directory without image.json, such as a volume's own folder
	vol := WithBrowsableVolumes(ctx, env.Repository, root).(*browsableRoot).volumes[0] //nolint:forcetypeassert

	volRoot, err := vol.open()
	require.NoError(t, err)
	require.Same(t, volRoot, WithBrowsableVolumes(ctx, env.Repository, volRoot))
}

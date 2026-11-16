package cli

import (
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/internal/repotesting"
	"github.com/kopia/kopia/snapshot"
	"github.com/kopia/kopia/snapshot/image"
)

const testImageDiskGUID = "0f6ab3f2-1c3d-4e5f-8a9b-0c1d2e3f4a5b"

// createTestImage creates an image snapshot of a disk with one 9 MiB raw
// volume for the repository's user@host and returns its manifest.
func createTestImage(ctx context.Context, t *testing.T, env *repotesting.Environment) *snapshot.Manifest {
	t.Helper()

	imagePath, err := image.DiskSourcePath(testImageDiskGUID)
	require.NoError(t, err)

	data := append(bytes.Repeat([]byte{7}, 4<<20), make([]byte, 5<<20)...)

	man, _, err := image.Create(ctx, env.RepositoryWriter, &image.CreateOptions{
		Source: snapshot.SourceInfo{
			Host:     env.Repository.ClientOptions().Hostname,
			UserName: env.Repository.ClientOptions().Username,
			Path:     imagePath,
		},
		Layout: &image.Layout{
			DiskGUID:   testImageDiskGUID,
			SectorSize: 512,
			DiskSize:   1 << 30,
			Partitions: []image.Partition{{
				Number: 1, TypeGUID: image.PartitionTypeEFISystem, UniqueGUID: "11111111-2222-4333-8444-555555555551",
				Offset: 1 << 20, Length: 100 << 20, Name: "EFI system partition",
			}},
		},
		Volumes: []image.VolumeSource{{
			PartitionNumber: 1, Label: "esp", Kind: image.VolumeKindRaw, Size: int64(len(data)),
			Open: func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil },
		}},
	})
	require.NoError(t, err)

	return man
}

func TestGetLocalBackupPaths_skipsImageSources(t *testing.T) {
	ctx, env := repotesting.NewEnvironment(t, repotesting.FormatNotImportant)

	createTestImage(ctx, t, env)

	sources, err := snapshot.ListSources(ctx, env.Repository)
	require.NoError(t, err)
	require.Len(t, sources, 1, "the image source has a snapshot")

	paths, err := getLocalBackupPaths(ctx, env.Repository)
	require.NoError(t, err)
	require.Empty(t, paths, "snapshot create --all must not include image sources")
}

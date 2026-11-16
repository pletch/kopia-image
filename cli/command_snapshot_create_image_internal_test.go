package cli

import (
	"bytes"
	"io"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/internal/repotesting"
	"github.com/kopia/kopia/snapshot"
	"github.com/kopia/kopia/snapshot/image"
)

func TestGetLocalBackupPaths_skipsImageSources(t *testing.T) {
	ctx, env := repotesting.NewEnvironment(t, repotesting.FormatNotImportant)

	const diskGUID = "0f6ab3f2-1c3d-4e5f-8a9b-0c1d2e3f4a5b"

	imagePath, err := image.DiskSourcePath(diskGUID)
	require.NoError(t, err)

	data := bytes.Repeat([]byte{7}, 1<<20)

	_, _, err = image.Create(ctx, env.RepositoryWriter, &image.CreateOptions{
		Source: snapshot.SourceInfo{
			Host:     env.Repository.ClientOptions().Hostname,
			UserName: env.Repository.ClientOptions().Username,
			Path:     imagePath,
		},
		Layout: &image.Layout{
			DiskGUID:   diskGUID,
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

	sources, err := snapshot.ListSources(ctx, env.Repository)
	require.NoError(t, err)
	require.Len(t, sources, 1, "the image source has a snapshot")

	paths, err := getLocalBackupPaths(ctx, env.Repository)
	require.NoError(t, err)
	require.Empty(t, paths, "snapshot create --all must not include image sources")
}

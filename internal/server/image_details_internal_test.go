package server

import (
	"bytes"
	"context"
	"io"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/internal/repotesting"
	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/object"
	"github.com/kopia/kopia/snapshot"
	"github.com/kopia/kopia/snapshot/image"
)

// countingRepo counts the objects read from the repository.
type countingRepo struct {
	repo.Repository

	opens atomic.Int32
}

func (r *countingRepo) OpenObject(ctx context.Context, id object.ID) (object.Reader, error) {
	r.opens.Add(1)

	return r.Repository.OpenObject(ctx, id) //nolint:wrapcheck
}

func TestImageDetailsCache_readsOnce(t *testing.T) {
	ctx, env := repotesting.NewEnvironment(t, repotesting.FormatNotImportant)

	const diskGUID = "0f6ab3f2-1c3d-4e5f-8a9b-0c1d2e3f4a51"

	data := bytes.Repeat([]byte{1, 2, 3, 4}, 1<<12)

	path, err := image.DiskSourcePath(diskGUID)
	require.NoError(t, err)

	man, _, err := image.Create(ctx, env.RepositoryWriter, &image.CreateOptions{
		Source: snapshot.SourceInfo{Host: "h", UserName: "u", Path: path},
		Layout: &image.Layout{
			DiskGUID:   diskGUID,
			SectorSize: 512,
			DiskSize:   1 << 30,
			Partitions: []image.Partition{{
				Number: 1, TypeGUID: image.PartitionTypeBasicData, UniqueGUID: "11111111-2222-4333-8444-555555555551",
				Offset: 1 << 20, Length: int64(len(data)), Name: "data",
			}},
		},
		Volumes: []image.VolumeSource{{
			PartitionNumber: 1, Label: "D", Kind: image.VolumeKindRaw, Size: int64(len(data)),
			Open: func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil },
		}},
	})
	require.NoError(t, err)

	rep := &countingRepo{Repository: env.RepositoryWriter}

	var c imageDetailsCache

	l, md, err := c.get(ctx, rep, man)
	require.NoError(t, err)
	require.NotNil(t, l)
	require.Equal(t, int64(1<<30), l.DiskSize)
	require.NotNil(t, md)
	require.Equal(t, diskGUID, md.DiskGUID)

	opens := rep.opens.Load()
	require.Positive(t, opens)

	// the files of an image do not change: the second time reads nothing
	l2, md2, err := c.get(ctx, rep, man)
	require.NoError(t, err)
	require.Same(t, l, l2)
	require.Same(t, md, md2)
	require.Equal(t, opens, rep.opens.Load())

	// a deleted image is forgotten, and read again if it is ever listed
	c.keepOnly(nil)

	_, _, err = c.get(ctx, rep, man)
	require.NoError(t, err)
	require.Greater(t, rep.opens.Load(), opens)
}

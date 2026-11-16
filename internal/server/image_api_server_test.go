package server_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/internal/repotesting"
	"github.com/kopia/kopia/internal/servertesting"
	"github.com/kopia/kopia/internal/testutil"
	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/content"
	"github.com/kopia/kopia/snapshot"
	"github.com/kopia/kopia/snapshot/image"
)

// TestImageCreate_throughAPIServer creates an image through a repository
// server, as a computer that backs up over the network does. Contents sent
// to the server are not visible until flushed, unlike pending contents of a
// direct connection.
func TestImageCreate_throughAPIServer(t *testing.T) {
	ctx, env := repotesting.NewEnvironment(t, repotesting.FormatNotImportant)
	apiServerInfo := servertesting.StartServer(t, env, true)

	rep, err := servertesting.ConnectAndOpenAPIServer(t, ctx, apiServerInfo, repo.ClientOptions{
		Username: servertesting.TestUsername,
		Hostname: servertesting.TestHostname,
	}, content.CachingOptions{CacheDirectory: testutil.TempDirectory(t)}, servertesting.TestPassword, &repo.Options{})
	require.NoError(t, err)

	defer rep.Close(ctx)

	// three chunks and a partial one, unlike any data already stored
	data := make([]byte, 3*image.DefaultChunkSize+12288)

	_, err = rand.Read(data)
	require.NoError(t, err)

	const diskGUID = "0f6ab3f2-1c3d-4e5f-8a9b-0c1d2e3f4a53"

	path, err := image.DiskSourcePath(diskGUID)
	require.NoError(t, err)

	var man *snapshot.Manifest

	require.NoError(t, repo.WriteSession(ctx, rep, repo.WriteSessionOptions{Purpose: "image"}, func(ctx context.Context, w repo.RepositoryWriter) error {
		var cerr error

		man, _, cerr = image.Create(ctx, w, &image.CreateOptions{
			Source: snapshot.SourceInfo{Host: servertesting.TestHostname, UserName: servertesting.TestUsername, Path: path},
			Layout: &image.Layout{
				DiskGUID: diskGUID, SectorSize: 512, DiskSize: 1 << 30,
				Partitions: []image.Partition{{
					Number: 1, TypeGUID: image.PartitionTypeBasicData, UniqueGUID: "11111111-2222-4333-8444-555555555553",
					Offset: 1 << 20, Length: int64(len(data)), Name: "data",
				}},
			},
			Volumes: []image.VolumeSource{{
				PartitionNumber: 1, Label: "D", Kind: image.VolumeKindRaw, Size: int64(len(data)),
				Open: func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil },
			}},
		})

		return cerr
	}))

	mans, err := image.ListSnapshots(ctx, rep)
	require.NoError(t, err)
	require.Len(t, mans, 1)
	require.Equal(t, man.ID, mans[0].ID)

	md, err := image.ReadMetadata(ctx, rep, mans[0])
	require.NoError(t, err)
	require.Equal(t, int64(len(data)), md.Totals().BytesRead)
}

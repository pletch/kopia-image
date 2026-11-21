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

// createRawTestImage creates an image snapshot of the test disk whose one raw
// volume holds data.
func createRawTestImage(ctx context.Context, t *testing.T, env *repotesting.Environment, data []byte) *snapshot.Manifest {
	t.Helper()

	imagePath, err := image.DiskSourcePath(testImageDiskGUID)
	require.NoError(t, err)

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
		ChunkSize: testImageChunkSize,
	})
	require.NoError(t, err)

	return man
}

// testImageChunkSize is the chunk size of the test images, which the expected
// figures are for.
const testImageChunkSize = 4 << 20

func TestImageCompare(t *testing.T) {
	ctx, env := repotesting.NewEnvironment(t, repotesting.FormatNotImportant)

	const chunk = testImageChunkSize

	data := make([]byte, 3*chunk)
	for i := range data {
		data[i] = byte(i * 7 % 251)
	}

	older := createRawTestImage(ctx, t, env, data)

	// one 4 KiB unit of the second chunk changes
	data[chunk+8192] ^= 0xFF
	newer := createRawTestImage(ctx, t, env, data)

	run := func(c *commandImageCompare) (string, error) {
		return captureStdout(t, func(out textOutput) error {
			c.out = out
			c.files = true
			c.depth = 5

			return c.run(ctx, env.Repository)
		})
	}

	// the order of the arguments does not matter
	out, err := run(&commandImageCompare{older: string(newer.ID), newer: string(older.ID), seed: 3})
	require.NoError(t, err)

	t.Log(out)
	require.Contains(t, out, "old  "+formatTimestamp(older.StartTime.ToTime())+"  "+string(older.ID))
	require.Contains(t, out, "new  "+formatTimestamp(newer.StartTime.ToTime())+"  "+string(newer.ID))
	require.Contains(t, out, "repeat with --seed=3")
	require.Contains(t, out, "p1-esp.img (raw, 3 chunks of 4 MiB)")
	require.Contains(t, out, "unchanged 2, moved 0, new 1 (4.2 MB)")
	require.Contains(t, out, "about 4.1 KB changed")
	require.Regexp(t, `4 MiB\s+4\.2 MB\s+100%\s+3\n`, out)
	require.Regexp(t, `64 KiB\s+65\.5 KB\s+1%\s+192\n`, out)
	require.Contains(t, out, "folders not attributed: not an NTFS volume")

	// the latest image of the source
	out, err = run(&commandImageCompare{older: string(older.ID), newer: newer.Source.String()})
	require.NoError(t, err)
	require.Contains(t, out, "new 1")

	_, err = run(&commandImageCompare{older: string(older.ID), newer: string(older.ID)})
	require.ErrorContains(t, err, "same image")
}

func TestGroupDigits(t *testing.T) {
	for n, want := range map[int64]string{0: "0", 999: "999", 1000: "1,000", 238415: "238,415", 1234567: "1,234,567", -1234: "-1,234", -123: "-123"} {
		require.Equal(t, want, groupDigits(n))
	}
}

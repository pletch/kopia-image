package cli

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/internal/repotesting"
)

func TestImageCheckCommand(t *testing.T) {
	ctx, env := repotesting.NewEnvironment(t, repotesting.FormatNotImportant)

	first := createTestImage(ctx, t, env)
	second := createTestImage(ctx, t, env)

	// by default, the latest image of this host's sources
	c := &commandImageCheck{samplePct: 1, parallel: 4}
	mans, err := c.snapshotsToCheck(ctx, env.Repository)
	require.NoError(t, err)
	require.Len(t, mans, 1)
	require.Equal(t, second.ID, mans[0].ID)
	require.NoError(t, c.run(ctx, env.Repository))

	// --all checks every image, latest first
	c = &commandImageCheck{all: true, full: true, parallel: 4}
	mans, err = c.snapshotsToCheck(ctx, env.Repository)
	require.NoError(t, err)
	require.Len(t, mans, 2)
	require.Equal(t, first.ID, mans[1].ID)
	require.NoError(t, c.run(ctx, env.Repository))

	// by snapshot ID
	c = &commandImageCheck{snapshots: []string{string(first.ID)}, full: true}
	mans, err = c.snapshotsToCheck(ctx, env.Repository)
	require.NoError(t, err)
	require.Len(t, mans, 1)
	require.Equal(t, first.ID, mans[0].ID)

	// a missing pack fails the check
	cids, err := env.Repository.VerifyObject(ctx, first.RootEntry.ObjectID)
	require.NoError(t, err)

	ci, err := env.Repository.ContentInfo(ctx, cids[0])
	require.NoError(t, err)

	require.NoError(t, env.RepositoryWriter.BlobStorage().DeleteBlob(ctx, ci.PackBlobID))

	c = &commandImageCheck{all: true, full: true, parallel: 4}
	require.ErrorContains(t, c.run(ctx, env.Repository), "failed the check")

	c = &commandImageCheck{samplePct: 150}
	require.ErrorContains(t, c.run(ctx, env.Repository), "between 0 and 100")
}

func TestImageCheckNTFS(t *testing.T) {
	ctx, env := repotesting.NewEnvironment(t, repotesting.FormatNotImportant)

	good := createNTFSTestImage(ctx, t, env)
	bad := createNTFSTestImageWith(ctx, t, env, func(b []byte) {
		b[len(b)-512+0x50]++ // the backup boot sector
	})

	check := func(id string, ntfs bool) (string, error) {
		return captureStdout(t, func(out textOutput) error {
			return (&commandImageCheck{snapshots: []string{id}, full: true, parallel: 4, ntfs: ntfs, out: out}).run(ctx, env.Repository)
		})
	}

	out, err := check(good, true)
	require.NoError(t, err)
	require.Contains(t, out, "p2-C.img file system: OK, 11 files, 9 directories")
	require.NotContains(t, out, "p1-esp.img file system", "raw volumes have no file system check")

	// the damage is only found by the file system check
	out, err = check(bad, false)
	require.NoError(t, err)
	require.NotContains(t, out, "file system")

	out, err = check(bad, true)
	require.ErrorContains(t, err, "1 of 1 image snapshots failed the check")
	require.Contains(t, out, "p2-C.img file system: FAILED")
	require.Contains(t, out, "the backup boot sector at the end of the volume does not match the boot sector")
}

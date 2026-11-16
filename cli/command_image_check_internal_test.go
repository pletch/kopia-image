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

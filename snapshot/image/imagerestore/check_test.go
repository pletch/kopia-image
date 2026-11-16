package imagerestore

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/internal/gather"
	"github.com/kopia/kopia/repo/blob"
)

func TestCheck_healthy(t *testing.T) {
	ctx, env, s, _ := createImageSnapshot(t)

	results, err := Check(ctx, env.Repository, s, CheckOptions{Sample: 1, Parallel: 4})
	require.NoError(t, err)
	require.Len(t, results, 2)

	for _, r := range results {
		require.True(t, r.OK(), "%v: %v", r.Volume.EntryName, r.Problems)
		require.True(t, r.PacksChecked)
		require.Equal(t, r.DistinctChunks, r.FetchedChunks)
	}

	// the NTFS image has 11 chunks, 6 of them distinct
	require.Equal(t, 11, results[1].Chunks)
	require.Equal(t, 6, results[1].DistinctChunks)
}

func TestCheck_sample(t *testing.T) {
	ctx, env, s, _ := createImageSnapshot(t)

	// 1% of 6 distinct chunks still checks one
	results, err := Check(ctx, env.Repository, s, CheckOptions{Sample: 0.01, Seed: 42})
	require.NoError(t, err)
	require.Equal(t, 1, results[1].FetchedChunks)

	results, err = Check(ctx, env.Repository, s, CheckOptions{Sample: 0.5, Seed: 42})
	require.NoError(t, err)
	require.Equal(t, 3, results[1].FetchedChunks)

	results, err = Check(ctx, env.Repository, s, CheckOptions{Sample: 0})
	require.NoError(t, err)
	require.Zero(t, results[1].FetchedChunks)
	require.True(t, results[1].OK(), "the pack check still runs")

	_, err = Check(ctx, env.Repository, s, CheckOptions{Sample: 2})
	require.Error(t, err)
}

func TestCheck_detectsDamagedPacks(t *testing.T) {
	for _, damage := range []string{"deleted", "truncated"} {
		t.Run(damage, func(t *testing.T) {
			ctx, env, s, _ := createImageSnapshot(t)

			cids, err := env.Repository.VerifyObject(ctx, s.Metadata.Volumes[1].ObjectID)
			require.NoError(t, err)

			ci, err := env.Repository.ContentInfo(ctx, cids[len(cids)-1])
			require.NoError(t, err)

			st := env.RepositoryWriter.BlobStorage()

			if damage == "deleted" {
				require.NoError(t, st.DeleteBlob(ctx, ci.PackBlobID))
			} else {
				require.NoError(t, st.PutBlob(ctx, ci.PackBlobID, gather.FromSlice([]byte("short")), blob.PutOptions{}))
			}

			results, err := Check(ctx, env.Repository, s, CheckOptions{Sample: 1})
			require.NoError(t, err)

			r := results[1]
			require.False(t, r.OK())
			require.True(t, r.PacksChecked)
			t.Logf("problems (%v): %v", r.ProblemCount, r.Problems)
			require.Contains(t, r.Problems[0], string(ci.PackBlobID))
		})
	}
}

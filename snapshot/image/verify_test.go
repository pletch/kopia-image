package image

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/internal/repotesting"
	"github.com/kopia/kopia/repo/content"
	"github.com/kopia/kopia/repo/object"
)

func TestVerifyObject(t *testing.T) {
	ctx, env := repotesting.NewEnvironment(t, repotesting.FormatNotImportant)

	data := append(randomBytes(4, 3*DefaultChunkSize), make([]byte, 2*DefaultChunkSize)...)

	w := env.RepositoryWriter.NewObjectWriter(ctx, object.WriterOptions{Splitter: imageSplitter})
	_, err := w.Write(data)
	require.NoError(t, err)

	oid, err := w.Result()
	require.NoError(t, err)
	require.NoError(t, w.Close())
	require.NoError(t, env.RepositoryWriter.Flush(ctx))

	for _, parallel := range []int{0, 1, 64} {
		var done, total int64

		infos, err := VerifyObject(ctx, env.RepositoryWriter, oid, parallel, func(d, tot int64) {
			done, total = max(done, d), tot
		})
		require.NoError(t, err)
		require.Equal(t, int64(5), total)
		require.Equal(t, int64(5), done)

		// the chunk list, 3 chunks of data and 1 of zeros
		require.Len(t, infos, 5)
	}

	// a chunk the repository does not hold
	missing := object.DirectObjectID(mustContentID(t, "0123456789abcdef0123456789abcdef"))
	_, err = VerifyObject(ctx, env.RepositoryWriter, missing, 8, nil)
	require.Error(t, err)

	// a chunk marked deleted is still readable but will be dropped by maintenance.
	ids, err := objectContents(ctx, env.RepositoryWriter, oid)
	require.NoError(t, err)
	require.NoError(t, env.RepositoryWriter.ContentManager().DeleteContent(ctx, ids[len(ids)-1]))
	require.NoError(t, env.RepositoryWriter.Flush(ctx))

	_, err = VerifyObject(ctx, env.RepositoryWriter, oid, 8, nil)
	require.ErrorContains(t, err, "marked deleted")
}

func mustContentID(t *testing.T, s string) content.ID {
	t.Helper()

	id, err := content.ParseID(s)
	require.NoError(t, err)

	return id
}

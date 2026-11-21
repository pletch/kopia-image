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
		infos, err := VerifyObject(ctx, env.RepositoryWriter, oid, parallel)
		require.NoError(t, err)

		// the chunk list, 3 chunks of data and 1 of zeros
		require.Len(t, infos, 5)
	}

	// a chunk the repository does not hold
	missing := object.DirectObjectID(mustContentID(t, "0123456789abcdef0123456789abcdef"))
	_, err = VerifyObject(ctx, env.RepositoryWriter, missing, 8)
	require.Error(t, err)
}

func mustContentID(t *testing.T, s string) content.ID {
	t.Helper()

	id, err := content.ParseID(s)
	require.NoError(t, err)

	return id
}

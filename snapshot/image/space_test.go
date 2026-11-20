package image

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/internal/repotesting"
	"github.com/kopia/kopia/snapshot"
)

func TestCalculateSpace(t *testing.T) {
	ctx, env := repotesting.NewEnvironment(t, repotesting.FormatNotImportant)

	create := func(data []byte) *snapshot.Manifest {
		t.Helper()

		opt := testCreateOptions(t, nil, data)
		opt.Volumes = opt.Volumes[1:] // the raw volume only
		opt.Compressor = ""

		man, _, err := Create(ctx, env.RepositoryWriter, opt)
		require.NoError(t, err)

		return man
	}

	// three chunks of random data, which does not compress
	first := randomBytes(1, 3*DefaultChunkSize)

	// the next image changes the last chunk only
	second := append(append([]byte{}, first[:2*DefaultChunkSize]...), randomBytes(2, DefaultChunkSize)...)

	m1 := create(first)
	m2 := create(second)

	require.NoError(t, env.RepositoryWriter.Flush(ctx))

	one, err := CalculateSpace(ctx, env.RepositoryWriter, []*snapshot.Manifest{m1})
	require.NoError(t, err)

	// alone, an image frees all it uses
	require.Equal(t, one.StoredBytes, one.ExclusiveBytes[m1.ID])
	require.Greater(t, one.StoredBytes, int64(3*DefaultChunkSize))
	require.Less(t, one.StoredBytes, int64(3*DefaultChunkSize+DefaultChunkSize/4))

	both, err := CalculateSpace(ctx, env.RepositoryWriter, []*snapshot.Manifest{m1, m2})
	require.NoError(t, err)

	// four distinct chunks, plus each image's small files
	require.Greater(t, both.StoredBytes, int64(4*DefaultChunkSize))
	require.Less(t, both.StoredBytes, int64(4*DefaultChunkSize+DefaultChunkSize/4))

	// each image alone uses one chunk: its last
	for _, m := range []*snapshot.Manifest{m1, m2} {
		require.Greater(t, both.ExclusiveBytes[m.ID], int64(DefaultChunkSize), m.ID)
		require.Less(t, both.ExclusiveBytes[m.ID], int64(DefaultChunkSize+DefaultChunkSize/4), m.ID)
	}
}

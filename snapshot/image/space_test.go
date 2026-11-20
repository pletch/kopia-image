package image

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/fs"
	"github.com/kopia/kopia/internal/repotesting"
	"github.com/kopia/kopia/repo/content"
	"github.com/kopia/kopia/repo/object"
	"github.com/kopia/kopia/snapshot"
	"github.com/kopia/kopia/snapshot/snapshotfs"
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

func TestImageContents_matchesVerifyObject(t *testing.T) {
	ctx, env := repotesting.NewEnvironment(t, repotesting.FormatNotImportant)

	opt := testCreateOptions(t, testVolumeData(), randomBytes(3, 2*DefaultChunkSize+12345))

	man, _, err := Create(ctx, env.RepositoryWriter, opt)
	require.NoError(t, err)
	require.NoError(t, env.RepositoryWriter.Flush(ctx))

	got, err := Contents(ctx, env.RepositoryWriter, man)
	require.NoError(t, err)

	// what kopia's own verification finds, object by object
	want := map[content.ID]bool{}

	tw, err := snapshotfs.NewTreeWalker(ctx, snapshotfs.TreeWalkerOptions{
		EntryCallback: func(ctx context.Context, _ fs.Entry, oid object.ID, _ string) error {
			cids, err := env.RepositoryWriter.VerifyObject(ctx, oid)
			for _, c := range cids {
				want[c] = true
			}

			return err
		},
		Parallelism: 1,
	})
	require.NoError(t, err)

	defer tw.Close(ctx)

	root, err := snapshotfs.SnapshotRoot(env.RepositoryWriter, man)
	require.NoError(t, err)
	require.NoError(t, tw.Process(ctx, root, "image"))

	gotSet := map[content.ID]bool{}
	for _, c := range got {
		gotSet[c] = true
	}

	require.Len(t, got, len(gotSet), "contents are listed once")
	require.Equal(t, want, gotSet)
}

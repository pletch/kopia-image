package server_test

import (
	"context"
	"crypto/rand"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/internal/repotesting"
	"github.com/kopia/kopia/internal/servertesting"
	"github.com/kopia/kopia/internal/testutil"
	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/content"
	"github.com/kopia/kopia/repo/object"
)

// A client writing through the server must un-delete a soft-deleted content,
// as a direct writer does, instead of skipping it because it "exists".
func TestServerWriteUndeletesSoftDeletedContent(t *testing.T) {
	ctx, env := repotesting.NewEnvironment(t, repotesting.FormatNotImportant)

	// large enough for the client's existence check.
	data := make([]byte, 100_000)
	_, err := rand.Read(data)
	require.NoError(t, err)

	w := env.RepositoryWriter.NewObjectWriter(ctx, object.WriterOptions{})
	_, err = w.Write(data)
	require.NoError(t, err)

	oid, err := w.Result()
	require.NoError(t, err)
	require.NoError(t, w.Close())

	cid, _, ok := oid.ContentID()
	require.True(t, ok)
	require.NoError(t, env.RepositoryWriter.Flush(ctx))

	require.NoError(t, env.RepositoryWriter.ContentManager().DeleteContent(ctx, cid))
	require.NoError(t, env.RepositoryWriter.Flush(ctx))

	ci, err := env.RepositoryWriter.ContentInfo(ctx, cid)
	require.NoError(t, err)
	require.True(t, ci.Deleted)

	apiServerInfo := servertesting.StartServer(t, env, true)

	rep, err := servertesting.ConnectAndOpenAPIServer(t, ctx, apiServerInfo, repo.ClientOptions{
		Username: servertesting.TestUsername,
		Hostname: servertesting.TestHostname,
	}, content.CachingOptions{
		CacheDirectory: testutil.TempDirectory(t),
	}, servertesting.TestPassword, &repo.Options{})
	require.NoError(t, err)

	defer rep.Close(ctx)

	require.NoError(t, repo.WriteSession(ctx, rep, repo.WriteSessionOptions{Purpose: "test"}, func(ctx context.Context, rw repo.RepositoryWriter) error {
		ow := rw.NewObjectWriter(ctx, object.WriterOptions{})
		defer ow.Close()

		if _, werr := ow.Write(data); werr != nil {
			return werr
		}

		oid2, rerr := ow.Result()
		require.Equal(t, oid, oid2)

		return rerr
	}))

	require.NoError(t, env.RepositoryWriter.Refresh(ctx))

	ci, err = env.RepositoryWriter.ContentInfo(ctx, cid)
	require.NoError(t, err)
	require.False(t, ci.Deleted, "writing through the server must un-delete the content")
}

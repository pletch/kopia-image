package server_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"io/fs"
	"path/filepath"
	"strings"
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

func TestServerClientReadWithoutDataCache(t *testing.T) {
	ctx, env := repotesting.NewEnvironment(t, repotesting.FormatNotImportant)

	data := make([]byte, 100_000)
	_, err := rand.Read(data)
	require.NoError(t, err)

	w := env.RepositoryWriter.NewObjectWriter(ctx, object.WriterOptions{})
	_, err = w.Write(data)
	require.NoError(t, err)

	oid, err := w.Result()
	require.NoError(t, err)
	require.NoError(t, w.Close())
	require.NoError(t, env.RepositoryWriter.Flush(ctx))

	cid, _, ok := oid.ContentID()
	require.True(t, ok)

	apiServerInfo := servertesting.StartServer(t, env, true)
	cacheDir := testutil.TempDirectory(t)

	rep, err := servertesting.ConnectAndOpenAPIServer(t, ctx, apiServerInfo, repo.ClientOptions{
		Username: servertesting.TestUsername,
		Hostname: servertesting.TestHostname,
	}, content.CachingOptions{
		CacheDirectory:        cacheDir,
		ContentCacheSizeBytes: 100 << 20,
	}, servertesting.TestPassword, &repo.Options{})
	require.NoError(t, err)

	defer rep.Close(ctx)

	cr, ok := rep.(interface {
		GetContent(ctx context.Context, id content.ID) ([]byte, error)
	})
	require.True(t, ok)

	got, err := cr.GetContent(content.WithoutDataCache(ctx), cid)
	require.NoError(t, err)
	require.Equal(t, data, got)
	require.Zero(t, cachedFileCount(t, cacheDir), "a bypassed read must not be cached")

	got, err = cr.GetContent(ctx, cid)
	require.NoError(t, err)
	require.Equal(t, data, got)
	require.NotZero(t, cachedFileCount(t, cacheDir), "a normal read is cached")
}

func cachedFileCount(t *testing.T, dir string) int {
	t.Helper()

	n := 0

	require.NoError(t, filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && !strings.HasPrefix(d.Name(), ".") {
			n++
		}

		return err
	}))

	return n
}

// The session stream is compressed on the wire: the server understands the
// client's codec, so the client never falls back.
func TestServerClientUsesWireCompression(t *testing.T) {
	ctx, env := repotesting.NewEnvironment(t, repotesting.FormatNotImportant)

	apiServerInfo := servertesting.StartServer(t, env, true)

	rep, err := servertesting.ConnectAndOpenAPIServer(t, ctx, apiServerInfo, repo.ClientOptions{
		Username: servertesting.TestUsername,
		Hostname: servertesting.TestHostname,
	}, content.CachingOptions{CacheDirectory: testutil.TempDirectory(t)}, servertesting.TestPassword, &repo.Options{})
	require.NoError(t, err)

	defer rep.Close(ctx)

	data := bytes.Repeat([]byte("compressible "), 100_000)

	require.NoError(t, repo.WriteSession(ctx, rep, repo.WriteSessionOptions{Purpose: "test"}, func(ctx context.Context, rw repo.RepositoryWriter) error {
		ow := rw.NewObjectWriter(ctx, object.WriterOptions{})
		defer ow.Close()

		_, werr := ow.Write(data)

		return werr
	}))

	require.True(t, repo.WireCompressionInUse(rep))
}

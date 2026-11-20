package cli

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/internal/cache"
	"github.com/kopia/kopia/internal/testlogging"
	"github.com/kopia/kopia/repo"
)

// The recovery system caches on its RAM disk, which data filled within
// seconds of a restore: the media's connection must cache no data, and limit
// the metadata cache strictly.
func TestRecoveryCaching(t *testing.T) {
	ctx := testlogging.Context(t)

	b, err := json.Marshal(&repo.LocalConfig{Caching: recoveryCaching()})
	require.NoError(t, err)

	// as the recovery system reads the connection from the media
	var lc repo.LocalConfig

	require.NoError(t, json.Unmarshal(b, &lc))
	require.Zero(t, lc.Caching.ContentCacheSizeBytes)
	require.Equal(t, int64(recoveryMetaCacheBytes), lc.Caching.EffectiveMetadataCacheSizeBytes())
	require.Equal(t, int64(recoveryMetaCacheBytes), lc.Caching.MetadataCacheSizeLimitBytes)

	// no data cache: neither the one of a repository server connection ...
	dir := t.TempDir()

	st, err := cache.NewStorageOrNil(ctx, dir, lc.Caching.ContentCacheSizeBytes, "server-contents")
	require.NoError(t, err)
	require.Nil(t, st)

	// ... nor the one of a direct connection, which reads through instead
	cc, err := cache.NewContentCache(ctx, nil, cache.Options{
		BaseCacheDirectory: dir,
		CacheSubDir:        "contents",
		Sweep:              cache.SweepSettings{MaxSizeBytes: lc.Caching.ContentCacheSizeBytes},
	}, nil)
	require.NoError(t, err)
	require.NoDirExists(t, dir+"/contents")

	cc.Close(ctx)
}

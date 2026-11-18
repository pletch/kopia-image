package content

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/internal/blobtesting"
	"github.com/kopia/kopia/internal/cache"
	"github.com/kopia/kopia/internal/gather"
	"github.com/kopia/kopia/internal/testlogging"
	"github.com/kopia/kopia/internal/testutil"
)

// zeroFiles overwrites the files below dir with zeros of the same length, as
// restoring a disk image that left the cache out does. Disk images keep the
// committed index cache in "indexes", which is not verified when opened.
func zeroFiles(t *testing.T, dir string) int {
	t.Helper()

	n := 0

	require.NoError(t, filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if d.IsDir() {
			if d.Name() == "indexes" {
				return filepath.SkipDir
			}

			return nil
		}

		info, err := d.Info()
		if err != nil {
			return err
		}

		n++

		return os.WriteFile(path, make([]byte, info.Size()), 0o600)
	}))

	return n
}

// TestGetContentDamagedCache checks that contents read through a cache whose
// files were zeroed are read from storage instead, and that the damaged cache
// entries are dropped.
func (s *contentManagerSuite) TestGetContentDamagedCache(t *testing.T) {
	for _, hint := range []string{"blobs", "contents"} {
		t.Run(hint, func(t *testing.T) {
			ctx := testlogging.Context(t)
			st := blobtesting.NewMapStorage(blobtesting.DataMap{}, nil, nil)
			cd := testutil.TempDirectory(t)

			bm := s.newTestContentManagerWithTweaks(t, st, &contentManagerTestTweaks{
				CacheDirectory:         cd,
				ContentCacheSizeBytes:  100e6,
				MetadataCacheSizeBytes: 100e6,
			})
			defer bm.CloseShared(ctx)

			data := bytes.Repeat([]byte{1, 2, 3, 4, 5, 6}, 1e5)

			dataID := writeContentAndVerify(ctx, t, bm, data)

			metaID, err := bm.WriteContent(ctx, gather.FromSlice(data[1:]), "k", NoCompression)
			require.NoError(t, err)

			require.NoError(t, bm.Flush(ctx))

			ids := []ID{dataID, metaID}
			require.Equal(t, ids, bm.PrefetchContents(ctx, ids, hint))

			dataBlob := getContentInfo(t, bm, dataID).PackBlobID
			metaBlob := getContentInfo(t, bm, metaID).PackBlobID

			if hint == "blobs" {
				require.Contains(t, allCacheKeys(t, bm.contentCache.CacheStorage()), cache.BlobIDCacheKey(dataBlob))
				require.Contains(t, allCacheKeys(t, bm.metadataCache.CacheStorage()), cache.BlobIDCacheKey(metaBlob))
			}

			require.Positive(t, zeroFiles(t, cd))

			got, err := bm.GetContent(ctx, dataID)
			require.NoError(t, err)
			require.Equal(t, data, got)

			got, err = bm.GetContent(ctx, metaID)
			require.NoError(t, err)
			require.Equal(t, data[1:], got)

			// the damaged entries are gone: the next read caches a good copy
			require.NotContains(t, allCacheKeys(t, bm.contentCache.CacheStorage()), cache.BlobIDCacheKey(dataBlob))
			require.NotContains(t, allCacheKeys(t, bm.metadataCache.CacheStorage()), cache.BlobIDCacheKey(metaBlob))

			got, err = bm.GetContent(ctx, dataID)
			require.NoError(t, err)
			require.Equal(t, data, got)
		})
	}
}

// TestGetContentDamagedStorage checks that a content that is damaged in
// storage still fails to read.
func (s *contentManagerSuite) TestGetContentDamagedStorage(t *testing.T) {
	ctx := testlogging.Context(t)
	data := blobtesting.DataMap{}
	st := blobtesting.NewMapStorage(data, nil, nil)

	bm := s.newTestContentManagerWithTweaks(t, st, &contentManagerTestTweaks{})
	defer bm.CloseShared(ctx)

	id := writeContentAndVerify(ctx, t, bm, bytes.Repeat([]byte{9, 8, 7}, 1e4))
	require.NoError(t, bm.Flush(ctx))

	b := getContentInfo(t, bm, id).PackBlobID
	clear(data[b])

	_, err := bm.GetContent(ctx, id)
	require.Error(t, err)
}

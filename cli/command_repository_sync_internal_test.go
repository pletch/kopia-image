package cli

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pkg/errors"
	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/internal/blobtesting"
	"github.com/kopia/kopia/internal/gather"
	"github.com/kopia/kopia/internal/testlogging"
	"github.com/kopia/kopia/repo/blob"
	"github.com/kopia/kopia/repo/format"
)

const testSyncFormatBlob = `{"uniqueID":"c3luYy10ZXN0"}`

// syncRecordingStorage records puts and deletes in order and can fail puts
// or reads of chosen blobs.
type syncRecordingStorage struct {
	blob.Storage

	mu  sync.Mutex
	ops []string

	failPut  map[blob.ID]bool
	vanish   map[blob.ID]bool
	onListFn func(prefix blob.ID)
}

func (s *syncRecordingStorage) record(op string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.ops = append(s.ops, op)
}

func (s *syncRecordingStorage) recorded() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]string(nil), s.ops...)
}

func (s *syncRecordingStorage) PutBlob(ctx context.Context, id blob.ID, data blob.Bytes, opts blob.PutOptions) error {
	if s.failPut[id] {
		return errors.Errorf("injected put failure for %v", id)
	}

	err := s.Storage.PutBlob(ctx, id, data, opts)
	if err == nil {
		s.record("put " + string(id))
	}

	return err //nolint:wrapcheck
}

func (s *syncRecordingStorage) DeleteBlob(ctx context.Context, id blob.ID) error {
	s.record("delete " + string(id))

	return s.Storage.DeleteBlob(ctx, id) //nolint:wrapcheck
}

func (s *syncRecordingStorage) GetBlob(ctx context.Context, id blob.ID, offset, length int64, output blob.OutputBuffer) error {
	if s.vanish[id] {
		return blob.ErrBlobNotFound
	}

	return s.Storage.GetBlob(ctx, id, offset, length, output) //nolint:wrapcheck
}

func (s *syncRecordingStorage) ListBlobs(ctx context.Context, prefix blob.ID, cb func(blob.Metadata) error) error {
	if s.onListFn != nil {
		s.onListFn(prefix)
	}

	return s.Storage.ListBlobs(ctx, prefix, cb) //nolint:wrapcheck
}

type syncTestEnv struct {
	srcData blobtesting.DataMap
	dstData blobtesting.DataMap
	srcTime map[blob.ID]time.Time
	dstTime map[blob.ID]time.Time
	src     *syncRecordingStorage
	dst     *syncRecordingStorage
	cmd     *commandRepositorySyncTo
}

func newSyncTestEnv(src, dst blobtesting.DataMap) *syncTestEnv {
	e := &syncTestEnv{
		srcData: src,
		dstData: dst,
		srcTime: map[blob.ID]time.Time{},
		dstTime: map[blob.ID]time.Time{},
	}

	srcNow := func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }
	dstNow := func() time.Time { return time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC) }

	for id := range src {
		e.srcTime[id] = srcNow()
	}

	for id := range dst {
		e.dstTime[id] = dstNow()
	}

	e.src = &syncRecordingStorage{Storage: blobtesting.NewMapStorage(src, e.srcTime, srcNow)}
	e.dst = &syncRecordingStorage{Storage: blobtesting.NewMapStorage(dst, e.dstTime, dstNow)}
	e.cmd = &commandRepositorySyncTo{
		repositorySyncUpdate:      true,
		repositorySyncParallelism: 4,
		progress:                  &cliProgress{},
	}

	return e
}

func (e *syncTestEnv) run(t *testing.T, blobcfg format.BlobStorageConfiguration) error {
	t.Helper()

	return e.cmd.runSyncWithStorage(testlogging.Context(t), e.src, e.dst, blobcfg)
}

func testSyncSource() blobtesting.DataMap {
	return blobtesting.DataMap{
		format.KopiaRepositoryBlobID: []byte(testSyncFormatBlob),
		format.KopiaBlobCfgBlobID:    []byte("blobcfg"),
		"kopia.maintenance":          []byte("maintenance"),
		"p0001":                      []byte("pack-1"),
		"p0002":                      []byte("pack-2"),
		"q0001":                      []byte("special-pack"),
		"_log_0001":                  []byte("log"),
		"n0001":                      []byte("v0-index"),
		"m0001":                      []byte("compaction-log"),
		"xn0_0001":                   []byte("epoch-index"),
		"s0001":                      []byte("session"),
	}
}

func opIndex(t *testing.T, ops []string, op string) int {
	t.Helper()

	for i, o := range ops {
		if o == op {
			return i
		}
	}

	t.Fatalf("%q not found in %v", op, ops)

	return -1
}

func TestSyncTo_CopiesInDependencyOrder(t *testing.T) {
	e := newSyncTestEnv(testSyncSource(), blobtesting.DataMap{})

	require.NoError(t, e.run(t, format.BlobStorageConfiguration{}))
	require.Equal(t, e.srcData, e.dstData)

	ops := e.dst.recorded()

	maxIdx := func(ids ...blob.ID) int {
		m := -1
		for _, id := range ids {
			m = max(m, opIndex(t, ops, "put "+string(id)))
		}

		return m
	}

	minIdx := func(ids ...blob.ID) int {
		m := len(ops)
		for _, id := range ids {
			m = min(m, opIndex(t, ops, "put "+string(id)))
		}

		return m
	}

	data := []blob.ID{"p0001", "p0002", "q0001", "_log_0001"}
	index := []blob.ID{"n0001", "xn0_0001", "s0001"}
	indexMeta := []blob.ID{"m0001"}
	config := []blob.ID{format.KopiaBlobCfgBlobID, "kopia.maintenance"}

	require.Less(t, maxIdx(data...), minIdx(index...), "packs must land before indexes: %v", ops)
	require.Less(t, maxIdx(index...), minIdx(indexMeta...), "indexes must land before compaction logs: %v", ops)
	require.Less(t, maxIdx(indexMeta...), minIdx(config...), "indexes must land before config: %v", ops)
	require.Equal(t, "put "+format.KopiaRepositoryBlobID, ops[len(ops)-1], "format blob must be written last")
}

func TestSyncTo_DeletesOnlyAfterAllCopiesSucceed(t *testing.T) {
	src := testSyncSource()
	dst := blobtesting.DataMap{
		format.KopiaRepositoryBlobID: []byte(testSyncFormatBlob),
		"p-old":                      []byte("old-pack"),
		"xn0_old":                    []byte("old-index"),
	}

	e := newSyncTestEnv(src, dst)
	e.cmd.repositorySyncDelete = true
	e.dst.failPut = map[blob.ID]bool{"xn0_0001": true}

	require.ErrorContains(t, e.run(t, format.BlobStorageConfiguration{}), "injected put failure")

	for _, op := range e.dst.recorded() {
		require.False(t, strings.HasPrefix(op, "delete "), "nothing may be deleted after a failed copy, got %v", op)
	}

	require.Contains(t, e.dstData, blob.ID("p-old"))
	require.Contains(t, e.dstData, blob.ID("xn0_old"))

	// once the destination accepts the write, the stale blobs go, indexes before packs.
	e.dst.failPut = nil
	e.dst.ops = nil

	require.NoError(t, e.run(t, format.BlobStorageConfiguration{}))
	require.Equal(t, e.srcData, e.dstData)

	ops := e.dst.recorded()
	lastPut := -1

	for i, op := range ops {
		if strings.HasPrefix(op, "put ") {
			lastPut = i
		}
	}

	require.Less(t, lastPut, opIndex(t, ops, "delete xn0_old"))
	require.Less(t, opIndex(t, ops, "delete xn0_old"), opIndex(t, ops, "delete p-old"))
}

func TestSyncTo_RefusesDeleteWithoutDestinationFormatBlob(t *testing.T) {
	e := newSyncTestEnv(testSyncSource(), blobtesting.DataMap{"unrelated": []byte("x")})
	e.cmd.repositorySyncDelete = true

	require.ErrorContains(t, e.run(t, format.BlobStorageConfiguration{}), "refusing to use --delete")
	require.Empty(t, e.dst.recorded())
	require.Equal(t, blobtesting.DataMap{"unrelated": []byte("x")}, e.dstData)
}

func TestSyncTo_DryRunWritesNothing(t *testing.T) {
	e := newSyncTestEnv(testSyncSource(), blobtesting.DataMap{})
	e.cmd.repositorySyncDryRun = true

	require.NoError(t, e.run(t, format.BlobStorageConfiguration{}))
	require.Empty(t, e.dst.recorded())
	require.Empty(t, e.dstData)
}

func TestSyncTo_ComparesConfigBlobContents(t *testing.T) {
	src := testSyncSource()
	dst := testSyncSource()

	// same length, and the destination's timestamp is newer, but contents differ.
	dst[format.KopiaBlobCfgBlobID] = []byte("BLOBCFG")

	e := newSyncTestEnv(src, dst)

	require.NoError(t, e.run(t, format.BlobStorageConfiguration{}))
	require.Equal(t, []string{"put " + format.KopiaBlobCfgBlobID}, e.dst.recorded())
	require.Equal(t, e.srcData, e.dstData)
}

func TestSyncTo_CopiesWhenLengthDiffers(t *testing.T) {
	src := testSyncSource()
	dst := testSyncSource()
	dst["p0001"] = []byte("trunc")

	e := newSyncTestEnv(src, dst)

	require.NoError(t, e.run(t, format.BlobStorageConfiguration{}))
	require.Equal(t, []string{"put p0001"}, e.dst.recorded())
}

func TestSyncTo_VanishedSourceBlobFailsAndSkipsDeletes(t *testing.T) {
	dst := blobtesting.DataMap{
		format.KopiaRepositoryBlobID: []byte(testSyncFormatBlob),
		"p-old":                      []byte("old-pack"),
	}

	e := newSyncTestEnv(testSyncSource(), dst)
	e.cmd.repositorySyncDelete = true
	e.src.vanish = map[blob.ID]bool{"p0002": true}

	require.ErrorContains(t, e.run(t, format.BlobStorageConfiguration{}), "disappeared from the source")
	require.Contains(t, e.dstData, blob.ID("p-old"))
}

func TestSyncTo_DefersIndexesThatAppearDuringListing(t *testing.T) {
	e := newSyncTestEnv(testSyncSource(), blobtesting.DataMap{})

	// a snapshot writes an index after the index prefixes were listed.
	// a snapshot writes a pack and its index once the index prefixes were listed.
	var once sync.Once

	e.src.onListFn = func(prefix blob.ID) {
		if strings.HasPrefix(string(prefix), "p") {
			once.Do(func() {
				ctx := testlogging.Context(t)
				require.NoError(t, e.src.Storage.PutBlob(ctx, "p0late", gather.FromSlice([]byte("late-pack")), blob.PutOptions{}))
				require.NoError(t, e.src.Storage.PutBlob(ctx, "xn0_late", gather.FromSlice([]byte("late-index")), blob.PutOptions{}))
			})
		}
	}

	require.NoError(t, e.run(t, format.BlobStorageConfiguration{}))
	require.NotContains(t, e.dstData, blob.ID("xn0_late"))
	require.Contains(t, e.dstData, blob.ID("xn0_0001"))
}

func TestSyncTo_RetentionFallsBackWhenUnsupported(t *testing.T) {
	e := newSyncTestEnv(testSyncSource(), blobtesting.DataMap{})

	blobcfg := format.BlobStorageConfiguration{
		RetentionMode:   blob.Governance,
		RetentionPeriod: 24 * time.Hour,
	}

	// the map storage rejects retention options, like the filesystem provider.
	require.NoError(t, e.run(t, blobcfg))
	require.Equal(t, e.srcData, e.dstData)
	require.True(t, e.cmd.retentionUnsupported.Load())
}

func TestSyncTo_PutOptionsApplyRetentionToLockedPrefixes(t *testing.T) {
	c := &commandRepositorySyncTo{}
	blobcfg := format.BlobStorageConfiguration{RetentionMode: blob.Compliance, RetentionPeriod: time.Hour}

	require.Equal(t, blob.Compliance, c.putOptions("p0001", blob.Metadata{}, blobcfg).RetentionMode)
	require.Equal(t, blob.Compliance, c.putOptions("xn0_0001", blob.Metadata{}, blobcfg).RetentionMode)
	require.Equal(t, blob.Compliance, c.putOptions(format.KopiaRepositoryBlobID, blob.Metadata{}, blobcfg).RetentionMode)
	require.Empty(t, c.putOptions("s0001", blob.Metadata{}, blobcfg).RetentionMode)
	require.Empty(t, c.putOptions("p0001", blob.Metadata{}, format.BlobStorageConfiguration{}).RetentionMode)
}

func TestSyncListShards_CoverEveryBlobExactlyOnce(t *testing.T) {
	ids := []blob.ID{
		format.KopiaRepositoryBlobID, format.KopiaBlobCfgBlobID, "kopia.maintenance", "kopia.repository.backup.abc",
		"p0123abc-s1", "pfedcba", "q0", "qf00", "n123", "m123", "l123", "xn0_123", "xe1", "s123", "_log_123",
		"Zcustom", "9abc", ".hidden", "-dash",
	}

	shards := syncListShards("")

	for _, id := range ids {
		matches := 0

		for _, s := range shards {
			if strings.HasPrefix(string(id), string(s)) {
				matches++
			}
		}

		require.Equal(t, 1, matches, "blob %v must be listed by exactly one shard", id)
	}

	// the second source pass skips characters the first pass covers completely.
	firstPass := syncFirstPassChars()
	require.Equal(t, "nxsml", firstPass)

	for _, s := range syncListShards(firstPass) {
		require.False(t, strings.ContainsRune(firstPass, rune(s[0])), "shard %v should be skipped", s)
	}

	require.Contains(t, syncListShards(firstPass), blob.ID("k"), "kopia.* is only partly covered by the first pass")
}

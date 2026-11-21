package ntfsfs

import (
	"bytes"
	"context"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/internal/repotesting"
	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/object"
)

const raChunk = 1 << 20

func readAheadObject(ctx context.Context, t *testing.T, rep repo.RepositoryWriter, n int) ([]byte, object.ID) {
	t.Helper()

	data := make([]byte, n*raChunk)
	r := rand.New(rand.NewPCG(7, 7)) //nolint:gosec

	for i := range data {
		data[i] = byte(r.Uint32())
	}

	w := rep.NewObjectWriter(ctx, object.WriterOptions{Splitter: "FIXED-1M"})
	defer w.Close()

	_, err := w.Write(data)
	require.NoError(t, err)

	oid, err := w.Result()
	require.NoError(t, err)
	require.NoError(t, rep.Flush(ctx))

	return data, oid
}

// waitFetches waits until the background fetches settle and returns their
// count.
func waitFetches(t *testing.T, c *chunkReader) int64 {
	t.Helper()

	var last int64 = -1

	for range 100 {
		n := c.Fetches()

		c.mu.Lock()
		busy := len(c.inflight)
		c.mu.Unlock()

		if n == last && busy == 0 {
			return n
		}

		last = n

		time.Sleep(20 * time.Millisecond)
	}

	return last
}

func TestReadAhead_sequential(t *testing.T) {
	ctx, env := repotesting.NewEnvironment(t, repotesting.FormatNotImportant)
	data, oid := readAheadObject(ctx, t, env.RepositoryWriter, 80)

	c, err := newChunkReader(ctx, env.RepositoryWriter, oid, 0)
	require.NoError(t, err)
	require.Equal(t, 256, c.capacity, "256 MiB of 1 MiB chunks")
	require.Equal(t, 32, c.readAheadChunks, "32 MiB ahead")

	// a file copied in 64 KiB reads: after the first chunk, the next 32 are
	// fetched ahead
	buf := make([]byte, 64<<10)

	for off := int64(0); off < raChunk+len64(buf); off += len64(buf) {
		n, err := c.ReadAt(buf, off)
		require.NoError(t, err)
		require.Equal(t, data[off:off+int64(n)], buf[:n])
	}

	fetched := waitFetches(t, c)
	require.GreaterOrEqual(t, fetched, int64(1+16), "the read chunks and at least a full set of fetches ahead")
	require.LessOrEqual(t, fetched, int64(2+32), "no more than 32 chunks ahead")

	// reading on is served from what was fetched ahead, and fetches further
	for off := raChunk + len64(buf); off < 20*raChunk; off += len64(buf) {
		n, err := c.ReadAt(buf, off)
		require.NoError(t, err)
		require.True(t, bytes.Equal(data[off:off+int64(n)], buf[:n]), "offset %v", off)
	}

	require.LessOrEqual(t, waitFetches(t, c), int64(20+32))
}

func TestReadAhead_scatteredReadsFetchNothingAhead(t *testing.T) {
	ctx, env := repotesting.NewEnvironment(t, repotesting.FormatNotImportant)
	data, oid := readAheadObject(ctx, t, env.RepositoryWriter, 40)

	c, err := newChunkReader(ctx, env.RepositoryWriter, oid, 0)
	require.NoError(t, err)

	// metadata lookups: small reads all over the volume, none continuing
	// another
	buf := make([]byte, 4096)

	for _, chunk := range []int64{3, 17, 5, 30, 11, 8, 25} {
		off := chunk*raChunk + 8192
		_, err := c.ReadAt(buf, off)
		require.NoError(t, err)
		require.Equal(t, data[off:off+4096], buf)
	}

	require.Equal(t, int64(7), waitFetches(t, c))
}

func TestReadAhead_smallCache(t *testing.T) {
	ctx, env := repotesting.NewEnvironment(t, repotesting.FormatNotImportant)
	_, oid := readAheadObject(ctx, t, env.RepositoryWriter, 4)

	// as image check scans the MFT: read-ahead stays within half the cache
	c, err := newChunkReader(ctx, env.RepositoryWriter, oid, 8)
	require.NoError(t, err)
	require.Equal(t, 4, c.readAheadChunks)
}

func len64(b []byte) int64 { return int64(len(b)) }

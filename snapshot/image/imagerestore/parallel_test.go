package imagerestore

import (
	"bytes"
	"context"
	"io"
	"math/rand/v2"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/internal/repotesting"
	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/object"
)

const chunk = 4 << 20 // the chunk size of the objects that writeTestObject writes

// memDisk is an in-memory target that records zero calls.
type memDisk struct {
	mu    sync.Mutex
	data  []byte
	zeros int
}

func (m *memDisk) WriteAt(p []byte, off int64) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if off+int64(len(p)) > int64(len(m.data)) {
		return 0, io.ErrShortWrite
	}

	return copy(m.data[off:], p), nil
}

func (m *memDisk) zero(off, length int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	clear(m.data[off : off+length])
	m.zeros++

	return nil
}

func randomBytes(seed uint64, n int) []byte {
	r := rand.New(rand.NewPCG(seed, seed)) //nolint:gosec
	b := make([]byte, n)

	for i := range b {
		b[i] = byte(r.Uint32())
	}

	return b
}

// testImage returns 10 chunks plus a partial one: random chunks, a chunk
// repeated three times and four zero chunks.
func testImage() []byte {
	var b bytes.Buffer

	repeated := randomBytes(99, chunk)

	for i, kind := range []string{"r", "z", "z", "x", "r", "x", "z", "z", "x", "r"} {
		switch kind {
		case "r":
			b.Write(randomBytes(uint64(i), chunk))
		case "z":
			b.Write(make([]byte, chunk))
		case "x":
			b.Write(repeated)
		}
	}

	b.Write(randomBytes(1000, 12345))

	return b.Bytes()
}

func writeTestObject(ctx context.Context, t *testing.T, rep repo.RepositoryWriter, data []byte) object.ID {
	t.Helper()

	w := rep.NewObjectWriter(ctx, object.WriterOptions{Splitter: "FIXED-4M", Compressor: "zstd-fastest"})
	defer w.Close()

	_, err := w.Write(data)
	require.NoError(t, err)

	oid, err := w.Result()
	require.NoError(t, err)
	require.NoError(t, rep.Flush(ctx))

	return oid
}

func TestWriteObject(t *testing.T) {
	ctx, env := repotesting.NewEnvironment(t, repotesting.FormatNotImportant)

	data := testImage()
	oid := writeTestObject(ctx, t, env.RepositoryWriter, data)

	chunks, length, err := Chunks(ctx, env.Repository, oid)
	require.NoError(t, err)
	require.Len(t, chunks, 11)
	require.Equal(t, int64(len(data)), length)

	const base = 1 << 20

	for _, parallel := range []int{1, 4, 32} {
		disk := &memDisk{data: bytes.Repeat([]byte{0xee}, base+len(data)+100)}

		var lastProgress int64

		stats, err := WriteObject(ctx, env.Repository, oid, disk, base, Options{
			Parallel: parallel,
			Zero:     disk.zero,
			Progress: func(done int64) {
				disk.mu.Lock()
				lastProgress = max(lastProgress, done)
				disk.mu.Unlock()
			},
		})
		require.NoError(t, err)

		require.Equal(t, data, disk.data[base:base+len(data)], "parallel=%v", parallel)
		require.Equal(t, bytes.Repeat([]byte{0xee}, base), disk.data[:base], "bytes before base untouched")
		require.Equal(t, bytes.Repeat([]byte{0xee}, 100), disk.data[base+len(data):], "bytes after the object untouched")

		require.Equal(t, Stats{
			Bytes:         int64(len(data)),
			Chunks:        11,
			FetchedChunks: 6, // 3 random + 1 repeated + 1 zero + 1 partial
			FetchedBytes:  5*chunk + 12345,
			ZeroBytes:     4 * chunk,
		}, stats)

		// adjacent zero chunks are merged into two ranges
		require.Equal(t, 2, disk.zeros)
		require.Equal(t, int64(len(data)), lastProgress)
	}
}

func TestWriteObject_zeroModes(t *testing.T) {
	ctx, env := repotesting.NewEnvironment(t, repotesting.FormatNotImportant)

	data := testImage()
	oid := writeTestObject(ctx, t, env.RepositoryWriter, data)

	// without a Zero function, zeros are written
	disk := &memDisk{data: bytes.Repeat([]byte{0xee}, len(data))}

	_, err := WriteObject(ctx, env.Repository, oid, disk, 0, Options{})
	require.NoError(t, err)
	require.Equal(t, data, disk.data)

	// a zeroed target leaves zero chunks untouched
	disk = &memDisk{data: bytes.Repeat([]byte{0xee}, len(data))}

	_, err = WriteObject(ctx, env.Repository, oid, disk, 0, Options{TargetIsZeroed: true, Zero: disk.zero})
	require.NoError(t, err)
	require.Zero(t, disk.zeros)
	require.Equal(t, bytes.Repeat([]byte{0xee}, chunk), disk.data[chunk:2*chunk])
	require.Equal(t, data[:chunk], disk.data[:chunk])
}

func TestWriteObject_directObject(t *testing.T) {
	ctx, env := repotesting.NewEnvironment(t, repotesting.FormatNotImportant)

	data := randomBytes(5, 100000)
	oid := writeTestObject(ctx, t, env.RepositoryWriter, data)

	_, isIndirect := oid.IndexObjectID()
	require.False(t, isIndirect)

	disk := &memDisk{data: make([]byte, len(data))}

	stats, err := WriteObject(ctx, env.Repository, oid, disk, 0, Options{})
	require.NoError(t, err)
	require.Equal(t, data, disk.data)
	require.Equal(t, 1, stats.Chunks)
}

func TestWriteObject_errors(t *testing.T) {
	ctx, env := repotesting.NewEnvironment(t, repotesting.FormatNotImportant)

	oid := writeTestObject(ctx, t, env.RepositoryWriter, testImage())

	// the target is too small
	_, err := WriteObject(ctx, env.Repository, oid, &memDisk{data: make([]byte, chunk)}, 0, Options{})
	require.ErrorIs(t, err, io.ErrShortWrite)

	canceled, cancel := context.WithCancel(ctx)
	cancel()

	_, err = WriteObject(canceled, env.Repository, oid, &memDisk{data: make([]byte, 64<<20)}, 0, Options{})
	require.Error(t, err)
}

func TestOptions_parallelBoundedByMemory(t *testing.T) {
	chunks := make([]object.IndirectObjectEntry, 10000)
	for i := range chunks {
		chunks[i] = object.IndirectObjectEntry{Start: int64(i) * MaxChunkLength, Length: MaxChunkLength}
	}

	o := Options{Parallel: 100000}.withDefaults(chunks)
	require.Equal(t, int(maxBufferedBytes/MaxChunkLength), o.Parallel)

	o = Options{Parallel: 8}.withDefaults(chunks)
	require.Equal(t, 8, o.Parallel)
}

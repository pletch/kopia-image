package image

import (
	"bytes"
	"io"
	"sync"
	"testing"

	"github.com/pkg/errors"
	"github.com/stretchr/testify/require"
)

// recordingReaderAt serves data and fails the test if a read touches a
// forbidden extent.
type recordingReaderAt struct {
	t         *testing.T
	data      []byte
	forbidden []Extent

	mu    sync.Mutex
	reads []Extent
}

func (r *recordingReaderAt) ReadAt(p []byte, off int64) (int, error) {
	r.mu.Lock()
	r.reads = append(r.reads, Extent{off, int64(len(p))})
	r.mu.Unlock()

	for _, f := range r.forbidden {
		if off < f.End() && f.Start < off+int64(len(p)) {
			r.t.Errorf("read [%v,%v) touches masked extent %+v", off, off+int64(len(p)), f)
		}
	}

	if off >= int64(len(r.data)) {
		return 0, io.EOF
	}

	n := copy(p, r.data[off:])
	if n < len(p) {
		return n, io.EOF
	}

	return n, nil
}

func patternData(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7 + 3)
		if b[i] == 0 {
			b[i] = 1
		}
	}

	return b
}

func applyMask(data []byte, mask []Extent) []byte {
	out := bytes.Clone(data)

	for _, e := range mask {
		for i := e.Start; i < e.End() && i < int64(len(out)); i++ {
			out[i] = 0
		}
	}

	return out
}

func TestMaskedReader_bitmapVolumeWithTail(t *testing.T) {
	const (
		cs       = 4096
		clusters = 64
		size     = clusters*cs + 2048 // tail beyond the last whole cluster, like NTFS's backup boot sector
	)

	// clusters 0-3 used, 4-19 free, 20-21 used, 22-62 free, 63 used
	bitmap := []byte{0x0f, 0, 0b00110000, 0, 0, 0, 0, 0x80}

	free, err := FreeExtents(bitmap, clusters, cs)
	require.NoError(t, err)

	data := patternData(size)
	src := &recordingReaderAt{t: t, data: data, forbidden: free}

	r, err := NewMaskedReader(src, size, free)
	require.NoError(t, err)
	require.Equal(t, int64(size), r.Size())
	require.Equal(t, int64(57*cs), r.MaskedBytes())

	want := applyMask(data, free)

	got, err := io.ReadAll(r)
	require.NoError(t, err)
	require.Equal(t, want, got)
	require.Equal(t, int64(size-57*cs), r.SourceBytesRead())
	require.Equal(t, data[clusters*cs:], got[clusters*cs:], "tail must be read as-is")
}

func TestMaskedReader_chunkedReadAt(t *testing.T) {
	const size = 10000

	mask := []Extent{{100, 50}, {120, 200}, {999, 1}, {5000, 3000}, {9990, 1000}}
	data := patternData(size)
	want := applyMask(data, mask)

	normalized, err := NormalizeExtents(mask)
	require.NoError(t, err)

	r, err := NewMaskedReader(&recordingReaderAt{t: t, data: data, forbidden: normalized}, size, mask)
	require.NoError(t, err)

	for _, chunk := range []int{1, 7, 64, 333, 4096, size, size + 1} {
		var got []byte

		buf := make([]byte, chunk)

		for off := int64(0); ; {
			for i := range buf {
				buf[i] = 0xee // ensure masked bytes are explicitly zeroed
			}

			n, err := r.ReadAt(buf, off)
			got = append(got, buf[:n]...)
			off += int64(n)

			if errors.Is(err, io.EOF) {
				break
			}

			require.NoError(t, err)
			require.Equal(t, chunk, n)
		}

		require.Equal(t, want, got, "chunk=%v", chunk)
	}
}

func TestMaskedReader_partialLastChunk(t *testing.T) {
	data := patternData(1000)

	r, err := NewMaskedReader(&recordingReaderAt{t: t, data: data}, 1000, nil)
	require.NoError(t, err)

	buf := make([]byte, 300)

	n, err := r.ReadAt(buf, 900)
	require.ErrorIs(t, err, io.EOF)
	require.Equal(t, 100, n)
	require.Equal(t, data[900:], buf[:n])

	n, err = r.ReadAt(buf, 1000)
	require.ErrorIs(t, err, io.EOF)
	require.Zero(t, n)
}

func TestMaskedReader_maskBeyondSizeIsClipped(t *testing.T) {
	src := &recordingReaderAt{t: t, data: patternData(100)}

	r, err := NewMaskedReader(src, 100, []Extent{{90, 50}, {200, 10}})
	require.NoError(t, err)
	require.Equal(t, int64(10), r.MaskedBytes())

	got, err := io.ReadAll(r)
	require.NoError(t, err)
	require.Len(t, got, 100)
	require.Equal(t, make([]byte, 10), got[90:])
}

func TestMaskedReader_fullyMaskedReadsNothing(t *testing.T) {
	src := &recordingReaderAt{t: t, data: patternData(4096)}

	r, err := NewMaskedReader(src, 4096, []Extent{{0, 4096}})
	require.NoError(t, err)

	got, err := io.ReadAll(r)
	require.NoError(t, err)
	require.Equal(t, make([]byte, 4096), got)
	require.Empty(t, src.reads)
}

func TestMaskedReader_readsSplitAtMaskBoundaries(t *testing.T) {
	src := &recordingReaderAt{t: t, data: patternData(16 * 512)}

	r, err := NewMaskedReader(src, 16*512, []Extent{{4 * 512, 2 * 512}})
	require.NoError(t, err)

	_, err = r.ReadAt(make([]byte, 16*512), 0)
	require.NoError(t, err)
	require.Equal(t, []Extent{{0, 4 * 512}, {6 * 512, 10 * 512}}, src.reads)
}

func TestMaskedReader_shortSource(t *testing.T) {
	r, err := NewMaskedReader(&recordingReaderAt{t: t, data: patternData(500)}, 1000, nil)
	require.NoError(t, err)

	_, err = io.ReadAll(r)
	require.ErrorIs(t, err, io.ErrUnexpectedEOF)
}

func TestMaskedReader_sourceError(t *testing.T) {
	errBoom := errors.New("boom")

	r, err := NewMaskedReader(failingReaderAt{errBoom}, 1000, []Extent{{0, 10}})
	require.NoError(t, err)

	n, err := r.ReadAt(make([]byte, 100), 0)
	require.ErrorIs(t, err, errBoom)
	require.Equal(t, 10, n)
}

func TestNewMaskedReader_invalid(t *testing.T) {
	_, err := NewMaskedReader(nil, -1, nil)
	require.Error(t, err)

	_, err = NewMaskedReader(nil, 10, []Extent{{-1, 5}})
	require.Error(t, err)

	r, err := NewMaskedReader(nil, 10, nil)
	require.NoError(t, err)

	_, err = r.ReadAt(make([]byte, 1), -1)
	require.Error(t, err)
}

type failingReaderAt struct{ err error }

func (f failingReaderAt) ReadAt([]byte, int64) (int, error) { return 0, f.err }

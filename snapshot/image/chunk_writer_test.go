package image

import (
	"bytes"
	"io"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/internal/repotesting"
	"github.com/kopia/kopia/repo/compression"
	"github.com/kopia/kopia/repo/object"
)

// TestWriteImageObject_matchesObjectWriter checks that images written chunk
// by chunk get exactly the object ID a streaming FIXED-4M object writer
// produces for the same data, so restore, check, mount and garbage
// collection see ordinary kopia objects.
func TestWriteImageObject_matchesObjectWriter(t *testing.T) {
	const c = DefaultChunkSize

	zero := make([]byte, c)

	cases := map[string][]byte{
		"one byte":              {7},
		"one zero chunk":        make([]byte, c),
		"one chunk":             randomBytes(1, c),
		"zeros and data":        bytes.Join([][]byte{randomBytes(2, c), zero, zero, randomBytes(3, c), zero}, nil),
		"partial zero tail":     bytes.Join([][]byte{randomBytes(4, c), make([]byte, 12345)}, nil),
		"partial data tail":     bytes.Join([][]byte{zero, randomBytes(5, 12345)}, nil),
		"all zeros, many":       make([]byte, 9*c+512),
		"repeated data chunks":  bytes.Repeat(randomBytes(6, c), 5),
		"zero chunk with a bit": bytes.Join([][]byte{zero, append(make([]byte, c-1), 1)}, nil),
	}

	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			ctx, env := repotesting.NewEnvironment(t, repotesting.FormatNotImportant)

			for _, comp := range []struct{ data, meta string }{{"", ""}, {"zstd-fastest", "zstd-fastest"}} {
				opt := &CreateOptions{Compressor: compressionName(comp.data), MetadataCompressor: compressionName(comp.meta), AsyncWrites: 4}

				got, stats, err := opt.writeImageObject(ctx, env.RepositoryWriter, bytes.NewReader(data), int64(len(data)), "p3-C.img")
				require.NoError(t, err)

				w := env.RepositoryWriter.NewObjectWriter(ctx, object.WriterOptions{
					Splitter:           imageSplitter,
					Compressor:         opt.Compressor,
					MetadataCompressor: opt.MetadataCompressor,
				})

				_, err = w.Write(data)
				require.NoError(t, err)

				want, err := w.Result()
				require.NoError(t, err)
				require.NoError(t, w.Close())

				require.Equal(t, want, got, "compression %q", comp.data)
				require.Equal(t, int64(len(data)), stats.BytesRead)

				r, err := env.RepositoryWriter.OpenObject(ctx, got)
				require.NoError(t, err)

				back, err := io.ReadAll(r)
				require.NoError(t, err)
				require.NoError(t, r.Close())
				require.True(t, bytes.Equal(data, back))
			}
		})
	}
}

func TestWriteImageObject_zeroChunksWrittenOnce(t *testing.T) {
	ctx, env := repotesting.NewEnvironment(t, repotesting.FormatNotImportant)

	data := make([]byte, 20*DefaultChunkSize)
	opt := &CreateOptions{AsyncWrites: 4}

	before, _ := env.RepositoryWriter.ContentManager().Stats.HashedContent()

	_, stats, err := opt.writeImageObject(ctx, env.RepositoryWriter, bytes.NewReader(data), int64(len(data)), "p3-C.img")
	require.NoError(t, err)

	after, _ := env.RepositoryWriter.ContentManager().Stats.HashedContent()

	require.Equal(t, int64(20), stats.ZeroChunks)
	// one zero chunk and one chunk list are hashed instead of 20 chunks
	require.Equal(t, uint32(2), after-before)
}

func TestWriteImageObject_readError(t *testing.T) {
	ctx, env := repotesting.NewEnvironment(t, repotesting.FormatNotImportant)

	opt := &CreateOptions{AsyncWrites: 4}
	data := randomBytes(7, 3*DefaultChunkSize-10) // the volume ends mid-chunk

	_, _, err := opt.writeImageObject(ctx, env.RepositoryWriter, bytes.NewReader(data), int64(len(data))+1, "p3-C.img")
	require.ErrorIs(t, err, io.ErrUnexpectedEOF)

	_, _, err = opt.writeImageObject(ctx, env.RepositoryWriter, bytes.NewReader(nil), 0, "p3-C.img")
	require.Error(t, err)
}

func compressionName(s string) compression.Name {
	return compression.Name(s)
}

func TestWriteImageObject_maskedSourceSkipsMaskedChunks(t *testing.T) {
	ctx, env := repotesting.NewEnvironment(t, repotesting.FormatNotImportant)

	const c = DefaultChunkSize

	data := randomBytes(8, 6*c+4096)

	// chunk 1 and the end of chunk 3 through chunk 4 are free space
	mask := []Extent{{Start: c, Length: c}, {Start: 3*c + c/2, Length: c + c/2}}

	normalized, err := NormalizeExtents(mask)
	require.NoError(t, err)

	src := &recordingReaderAt{t: t, data: data, forbidden: normalized}

	mr, err := NewMaskedReader(src, int64(len(data)), mask)
	require.NoError(t, err)

	opt := &CreateOptions{Compressor: "zstd-fastest", MetadataCompressor: "zstd-fastest", AsyncWrites: 4}

	got, stats, err := opt.writeImageObject(ctx, env.RepositoryWriter, maskedReadCloser{mr}, int64(len(data)), "p3-C.img")
	require.NoError(t, err)
	require.Equal(t, int64(2), stats.ZeroChunks, "chunks 1 and 4")

	// the same object as the streaming writer produces for the masked bytes
	want := applyMask(data, mask)

	w := env.RepositoryWriter.NewObjectWriter(ctx, object.WriterOptions{
		Splitter: imageSplitter, Compressor: opt.Compressor, MetadataCompressor: opt.MetadataCompressor,
	})

	_, err = w.Write(want)
	require.NoError(t, err)

	wantID, err := w.Result()
	require.NoError(t, err)
	require.NoError(t, w.Close())
	require.Equal(t, wantID, got)

	// fully masked chunks were not read at all, partly masked ones only outside the mask
	for _, rd := range src.reads {
		require.False(t, rd.Start >= c && rd.End() <= 2*c, "read %+v of masked chunk 1", rd)
	}

	require.Equal(t, int64(len(data))-TotalLength(normalized), mr.SourceBytesRead())
}

type maskedReadCloser struct {
	*MaskedReader
}

func (maskedReadCloser) Close() error { return nil }

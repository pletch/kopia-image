package grpczstd

import (
	"bytes"
	"crypto/rand"
	"io"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/encoding"
)

func roundTrip(t *testing.T, c encoding.Compressor, data []byte) (compressed int) {
	t.Helper()

	var buf bytes.Buffer

	w, err := c.Compress(&buf)
	require.NoError(t, err)

	_, err = w.Write(data)
	require.NoError(t, err)
	require.NoError(t, w.Close())

	compressed = buf.Len()

	r, err := c.Decompress(&buf)
	require.NoError(t, err)

	got, err := io.ReadAll(r)
	require.NoError(t, err)
	require.Equal(t, data, got)

	if rc, ok := r.(io.Closer); ok {
		require.NoError(t, rc.Close())
	}

	return compressed
}

func TestRegisteredAndRoundTrips(t *testing.T) {
	c := encoding.GetCompressor(Name)
	require.NotNil(t, c)

	compressible := bytes.Repeat([]byte("kopia image chunk "), 64<<10)
	require.Less(t, roundTrip(t, c, compressible), len(compressible)/10)

	random := make([]byte, 1<<20)
	_, err := rand.Read(random)
	require.NoError(t, err)

	// pooled encoders and decoders are reused correctly
	for range 3 {
		roundTrip(t, c, random)
		roundTrip(t, c, compressible)
	}

	roundTrip(t, c, []byte{})
}

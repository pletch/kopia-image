package image

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// imageSplitter is the splitter of the default chunk size, with which tests
// write objects as a streaming object writer would.
const imageSplitter = "FIXED-1M"

func TestChunkSizes(t *testing.T) {
	s, err := splitterFor(DefaultChunkSize)
	require.NoError(t, err)
	require.Equal(t, imageSplitter, s)

	for name, size := range ChunkSizes {
		s, err := splitterFor(size)
		require.NoError(t, err)
		require.Equal(t, "FIXED-"+name, s)
		require.GreaterOrEqual(t, size, int64(MinChunkSize))
		require.LessOrEqual(t, size, int64(MaxChunkSize))
	}

	_, err = splitterFor(3 << 20)
	require.Error(t, err)

	// the same data in flight whatever the chunk size
	require.Equal(t, 16, ParallelChunks(4<<20))
	require.Equal(t, 64, ParallelChunks(1<<20))
	require.Equal(t, 64, ParallelChunks(0))
	require.Equal(t, 8, ParallelChunks(8<<20))
}

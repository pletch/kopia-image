package image

import (
	"github.com/pkg/errors"
)

// Chunk sizes of image objects. Every chunk i of an image covers bytes
// [i*ChunkSize, (i+1)*ChunkSize) of its volume, and an image records its chunk
// size in Metadata.ChunkSize.
//
// A chunk is stored again whenever any of its bytes changes, and Windows
// changes small pieces all over a disk: with 4 MiB chunks a nightly image of a
// system disk stored about ten times what changed, with 1 MiB chunks about
// half as much. Smaller chunks cost more index entries and more requests to a
// repository server, which more requests at once make up for; see
// ParallelChunks.
const (
	// DefaultChunkSize is the chunk size of new images. Images of kopia
	// v0.23.1-wails.39 and older have 4 MiB chunks.
	DefaultChunkSize = 1 << 20

	MinChunkSize = 128 << 10
	MaxChunkSize = 8 << 20

	// inFlightBytes is about how much image data is read or written at once:
	// 16 chunks of 4 MiB, 64 of 1 MiB.
	inFlightBytes = 64 << 20
)

// ChunkSizes are the chunk sizes an image can have, by their names, which
// are those of kopia's fixed-size splitters.
//
//nolint:gochecknoglobals,mnd
var ChunkSizes = map[string]int64{
	"128K": 128 << 10,
	"256K": 256 << 10,
	"512K": 512 << 10,
	"1M":   1 << 20,
	"2M":   2 << 20,
	"4M":   4 << 20,
	"8M":   8 << 20,
}

// splitterFor returns the name of the fixed-size splitter that writes chunks
// of the given size.
func splitterFor(chunkSize int64) (string, error) {
	for name, size := range ChunkSizes {
		if size == chunkSize {
			return "FIXED-" + name, nil
		}
	}

	return "", errors.Errorf("unsupported chunk size %v", chunkSize)
}

// ParallelChunks returns how many chunks of an image to read or write at once
// so that about the same amount of data is in flight whatever the chunk size:
// smaller chunks take more requests, each with the same round trip.
func ParallelChunks(chunkSize int64) int {
	if chunkSize <= 0 {
		chunkSize = DefaultChunkSize
	}

	return int(max(1, inFlightBytes/chunkSize))
}

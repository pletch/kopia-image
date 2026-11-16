package image

import (
	"bytes"
	"context"
	"encoding/json"
	stderrors "errors"
	"io"
	"sync"

	"github.com/pkg/errors"
	"golang.org/x/sync/errgroup"

	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/object"
)

// indexContentPrefix and indirectStreamType reproduce how kopia's object
// writer stores the chunk list of a multi-chunk object, so images written here
// get exactly the object ID a FIXED-4M object writer would produce.
const (
	indexContentPrefix = "x"
	indirectStreamType = "kopia:indirect"
)

// writeImageObject writes exactly size bytes from r as a FIXED-4M object.
//
// Instead of streaming the volume through one object writer, it writes each
// chunk as its own single-chunk object, in parallel, and then the chunk list
// in the object writer's own format, so the object ID is identical. That lets
// zero chunks skip all work: chunks that a masked source reports as free
// space are not read, cleared or compared, and other zero chunks are not
// hashed. Every zero chunk refers to one zero chunk object, written once per
// run and deduplicated across runs. On free space this is about 20 times
// faster than streaming, which copies and hashes every chunk.
func (opt *CreateOptions) writeImageObject(ctx context.Context, rep repo.RepositoryWriter, r io.Reader, size int64, name string) (object.ID, VolumeStats, error) {
	var stats VolumeStats

	if size <= 0 {
		return object.EmptyID, stats, errors.Errorf("invalid image size %v", size)
	}

	parallel := max(1, opt.AsyncWrites)
	buffers := opt.chunkBuffers(parallel + 1) // one per worker plus one being read

	// a source that knows its zeroed ranges lets zero chunks skip the read and compare
	masked, isMasked := r.(maskedSource)

	entries := make([]object.IndirectObjectEntry, (size+DefaultChunkSize-1)/DefaultChunkSize)
	zeros := &zeroChunkCache{opt: opt, rep: rep, name: name}

	eg, egctx := errgroup.WithContext(ctx)
	eg.SetLimit(parallel)

	readErr := func() error {
		for i, total := 0, int64(0); total < size; i++ {
			n := min(int64(DefaultChunkSize), size-total)
			entry := &entries[i]
			entry.Start, entry.Length = total, n

			stats.ChunksTotal++
			stats.BytesRead += n
			total += n

			if isMasked && masked.IsMasked(entry.Start, n) {
				if err := zeros.fill(egctx, entry, &stats); err != nil {
					return err
				}

				continue
			}

			var buf []byte

			select {
			case buf = <-buffers:
			case <-egctx.Done():
				return errors.Wrap(egctx.Err(), "imaging canceled")
			}

			chunk := buf[:n]

			if m, err := readChunk(r, masked, isMasked, chunk, entry.Start); err != nil {
				buffers <- buf
				return errors.Wrapf(err, "error reading volume at offset %v of %v", entry.Start+int64(m), size)
			}

			if isZeroChunk(chunk) {
				buffers <- buf

				if err := zeros.fill(egctx, entry, &stats); err != nil {
					return err
				}

				continue
			}

			eg.Go(func() error {
				defer func() { buffers <- buf }()

				oid, err := opt.writeChunk(egctx, rep, chunk, name)
				entry.Object = oid

				return err
			})
		}

		return nil
	}()

	if err := stderrors.Join(readErr, eg.Wait()); err != nil {
		return object.EmptyID, stats, err
	}

	oid, err := opt.writeChunkList(ctx, rep, entries, name)

	return oid, stats, err
}

// chunkBuffers returns a pool of n chunk buffers.
func (opt *CreateOptions) chunkBuffers(n int) chan []byte {
	newBuffer := opt.NewBuffer
	if newBuffer == nil {
		newBuffer = func(n int) []byte { return make([]byte, n) }
	}

	buffers := make(chan []byte, n)
	for range n {
		buffers <- newBuffer(DefaultChunkSize)
	}

	return buffers
}

// maskedSource is a volume reader that knows which ranges read as zeros,
// such as a MaskedReader.
type maskedSource interface {
	io.ReaderAt
	IsMasked(off, length int64) bool
}

// readChunk reads a whole chunk at off: by offset from a masked source, which
// lets masked chunks be skipped, otherwise sequentially.
func readChunk(r io.Reader, masked maskedSource, isMasked bool, chunk []byte, off int64) (int, error) {
	if !isMasked {
		return io.ReadFull(r, chunk) //nolint:wrapcheck
	}

	n, err := masked.ReadAt(chunk, off)
	if n == len(chunk) {
		return n, nil
	}

	if err == nil || errors.Is(err, io.EOF) {
		err = io.ErrUnexpectedEOF
	}

	return n, err
}

// writeChunk writes one chunk as a single-chunk object, whose ID is the ID of
// its content, as a multi-chunk object writer records it.
func (opt *CreateOptions) writeChunk(ctx context.Context, rep repo.RepositoryWriter, data []byte, name string) (object.ID, error) {
	w := rep.NewObjectWriter(ctx, object.WriterOptions{
		Description:        imageObjectDescription + name,
		Splitter:           imageSplitter,
		Compressor:         opt.Compressor,
		MetadataCompressor: opt.MetadataCompressor,
	})
	defer w.Close() //nolint:errcheck

	if _, err := w.Write(data); err != nil {
		return object.EmptyID, errors.Wrap(err, "error writing image chunk")
	}

	oid, err := w.Result()

	return oid, errors.Wrap(err, "error writing image chunk")
}

// writeChunkList writes the chunk list as kopia's object writer does when it
// finishes a multi-chunk object, and returns the object's ID.
func (opt *CreateOptions) writeChunkList(ctx context.Context, rep repo.RepositoryWriter, entries []object.IndirectObjectEntry, name string) (object.ID, error) {
	if len(entries) == 1 {
		return entries[0].Object, nil
	}

	w := rep.NewObjectWriter(ctx, object.WriterOptions{
		Description:        "LIST(" + imageObjectDescription + name + ")",
		Prefix:             indexContentPrefix,
		Compressor:         opt.MetadataCompressor,
		MetadataCompressor: opt.MetadataCompressor,
	})
	defer w.Close() //nolint:errcheck

	index := struct {
		Stream  string                       `json:"stream"`
		Entries []object.IndirectObjectEntry `json:"entries"`
	}{indirectStreamType, entries}

	if err := json.NewEncoder(w).Encode(index); err != nil {
		return object.EmptyID, errors.Wrap(err, "error writing the image chunk list")
	}

	indexID, err := w.Result()
	if err != nil {
		return object.EmptyID, errors.Wrap(err, "error writing the image chunk list")
	}

	oid, err := object.ParseID("I" + indexID.String())

	return oid, errors.Wrap(err, "invalid image object ID")
}

// zeroChunkCache writes the zero chunk of each length once.
type zeroChunkCache struct {
	opt  *CreateOptions        // +checklocksignore: set at creation
	rep  repo.RepositoryWriter // +checklocksignore: set at creation
	name string                // +checklocksignore: set at creation

	mu sync.Mutex
	// +checklocks:mu
	ids map[int64]object.ID
}

// fill points a zero chunk's entry at the zero chunk object and counts it.
func (z *zeroChunkCache) fill(ctx context.Context, entry *object.IndirectObjectEntry, stats *VolumeStats) error {
	oid, err := z.get(ctx, entry.Length)
	if err != nil {
		return err
	}

	entry.Object = oid
	stats.ZeroChunks++

	return nil
}

func (z *zeroChunkCache) get(ctx context.Context, length int64) (object.ID, error) {
	z.mu.Lock()
	defer z.mu.Unlock()

	if oid, ok := z.ids[length]; ok {
		return oid, nil
	}

	oid, err := z.opt.writeChunk(ctx, z.rep, zeroChunk[:length], z.name)
	if err != nil {
		return object.EmptyID, err
	}

	if z.ids == nil {
		z.ids = map[int64]object.ID{}
	}

	z.ids[length] = oid

	return oid, nil
}

var zeroChunk = make([]byte, DefaultChunkSize)

func isZeroChunk(b []byte) bool {
	return bytes.Equal(b, zeroChunk[:len(b)])
}

// Package imagerestore writes kopia images back to disks: it recreates the
// partition table and writes each volume image with many chunks fetched in
// parallel and written out of order, which restores large images at network
// speed instead of one round trip per chunk.
package imagerestore

import (
	"bytes"
	"context"
	"encoding/json"
	stderrors "errors"
	"io"
	"sync"
	"sync/atomic"

	"github.com/pkg/errors"
	"golang.org/x/sync/errgroup"

	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/object"
	"github.com/kopia/kopia/snapshot/image"
)

const (
	indirectStreamType = "kopia:indirect"
	maxIndexObjectSize = 1 << 30
	zeroWriteSize      = 4 << 20
	zeroRangePieceSize = 64 << 20
	defaultParallel    = 8
)

// MaxChunkLength is the longest chunk accepted in an image object. Images are
// written in chunks of image.DefaultChunkSize; the limit keeps a damaged or
// forged chunk list from making readers allocate arbitrarily large buffers.
const MaxChunkLength = 4 * image.DefaultChunkSize

// ErrUnexpectedLength is returned when an image object is not as long as the
// image metadata says, before anything is written.
var ErrUnexpectedLength = errors.New("image object has an unexpected length")

// Chunks returns the chunks of an object and its length. An indirect object's
// chunks are the entries of its index; a direct object is a single chunk. The
// chunks tile the object from offset 0 without gaps or overlaps.
func Chunks(ctx context.Context, rep repo.Repository, oid object.ID) ([]object.IndirectObjectEntry, int64, error) {
	indexID, isIndirect := oid.IndexObjectID()
	if !isIndirect {
		r, err := rep.OpenObject(ctx, oid)
		if err != nil {
			return nil, 0, errors.Wrapf(err, "unable to open %v", oid)
		}

		length := r.Length()

		if err := r.Close(); err != nil {
			return nil, 0, errors.Wrapf(err, "unable to close %v", oid)
		}

		if length > MaxChunkLength {
			return nil, 0, errors.Errorf("%v is a single chunk of %v bytes, longer than %v", oid, length, MaxChunkLength)
		}

		return []object.IndirectObjectEntry{{Start: 0, Length: length, Object: oid}}, length, nil
	}

	r, err := rep.OpenObject(ctx, indexID)
	if err != nil {
		return nil, 0, errors.Wrapf(err, "unable to open the index of %v", oid)
	}
	defer r.Close() //nolint:errcheck

	entries, length, err := decodeIndex(r)

	return entries, length, errors.Wrapf(err, "invalid index of %v", oid)
}

// decodeIndex reads the chunk list of an indirect object and checks that the
// chunks tile the object from offset 0 without gaps or overlaps and that none
// is longer than MaxChunkLength. It returns the chunks and the object length.
func decodeIndex(r io.Reader) ([]object.IndirectObjectEntry, int64, error) {
	var index struct {
		Stream  string                       `json:"stream"`
		Entries []object.IndirectObjectEntry `json:"entries"`
	}

	if err := json.NewDecoder(io.LimitReader(r, maxIndexObjectSize)).Decode(&index); err != nil {
		return nil, 0, errors.Wrap(err, "unable to decode")
	}

	if index.Stream != indirectStreamType {
		return nil, 0, errors.Errorf("stream type %q", index.Stream)
	}

	var end int64

	for i, e := range index.Entries {
		if e.Start != end || e.Length <= 0 || e.Length > MaxChunkLength {
			return nil, 0, errors.Errorf("entry %v starts at %v and is %v bytes long, expected to start at %v and be at most %v bytes", i, e.Start, e.Length, end, MaxChunkLength)
		}

		end += e.Length
	}

	return index.Entries, end, nil
}

// Options configure WriteObject.
type Options struct {
	// Parallel is the number of chunks fetched at once. It defaults to 8.
	Parallel int

	// ExpectedLength, when positive, is the length the object must have.
	// WriteObject returns ErrUnexpectedLength before writing anything if the
	// object is longer or shorter, so that a wrong object cannot be written
	// past the space reserved for it.
	ExpectedLength int64

	// Limit, when positive, is how much of the object to write: the rest
	// must be zeros up to DataEnd, such as the free end of an NTFS volume
	// restored to a shorter partition, and WriteObject fails if it is not.
	Limit int64

	// DataEnd, when positive, is where the data that Limit must not cut
	// ends: past it the object holds nothing to keep, such as the backup
	// boot sector after the last cluster of an NTFS volume.
	DataEnd int64

	// TargetIsZeroed skips writing zero chunks, for targets known to read as
	// zeros, such as new files or trimmed disks.
	TargetIsZeroed bool

	// Zero zeroes a target range quickly, e.g. with BLKZEROOUT. When nil,
	// zeros are written.
	Zero func(off, length int64) error

	// NewBuffer allocates chunk buffers, e.g. aligned for unbuffered writes.
	NewBuffer func(n int) []byte

	// Progress, when set, is called with the number of object bytes done.
	Progress func(done int64)
}

// Stats summarize a WriteObject call.
type Stats struct {
	// Bytes is the object length.
	Bytes int64

	// Chunks is the number of chunks, FetchedChunks the number of distinct
	// chunks fetched and FetchedBytes their size.
	Chunks        int
	FetchedChunks int
	FetchedBytes  int64

	// ZeroBytes is the size of the chunks consisting of zeros, which were
	// zeroed or skipped rather than written from fetched data.
	ZeroBytes int64
}

// WriteObject writes an object to w at offset base. Distinct chunks are
// fetched once, in parallel, and written at every offset where they occur,
// in any order. Zero chunks are fetched once and then zeroed with
// opt.Zero, or skipped when opt.TargetIsZeroed. Each fetched chunk is
// verified by the repository.
func WriteObject(ctx context.Context, rep repo.Repository, oid object.ID, w io.WriterAt, base int64, opt Options) (Stats, error) {
	chunks, length, err := Chunks(ctx, rep, oid)
	if err != nil {
		return Stats{}, err
	}

	if opt.ExpectedLength > 0 && length != opt.ExpectedLength {
		return Stats{}, errors.Wrapf(ErrUnexpectedLength, "%v is %v bytes, expected %v", oid, length, opt.ExpectedLength)
	}

	if opt.Limit <= 0 || opt.Limit > length {
		opt.Limit = length
	}

	if opt.DataEnd <= 0 || opt.DataEnd > length {
		opt.DataEnd = length
	}

	groups := groupChunks(chunks)
	stats := Stats{Bytes: length, Chunks: len(chunks), FetchedChunks: len(groups)}

	w2 := &objectWriter{rep: rep, w: w, base: base, opt: opt.withDefaults(chunks)}

	if err := w2.writeGroups(ctx, groups); err != nil {
		return stats, err
	}

	if err := w2.zeroRanges(ctx); err != nil {
		return stats, err
	}

	stats.FetchedBytes = w2.fetchedBytes.Load()
	stats.ZeroBytes = image.TotalLength(w2.zeros)

	return stats, nil
}

func (o Options) withDefaults(chunks []object.IndirectObjectEntry) Options {
	if o.Parallel <= 0 {
		o.Parallel = defaultParallel
	}

	if o.NewBuffer == nil {
		o.NewBuffer = func(n int) []byte { return make([]byte, n) }
	}

	if o.Progress == nil {
		o.Progress = func(int64) {}
	}

	o.Parallel = min(o.Parallel, max(1, len(chunks)))

	return o
}

// chunkGroup is a distinct chunk and every object offset where it occurs.
type chunkGroup struct {
	object  object.ID
	length  int64
	offsets []int64
}

func groupChunks(chunks []object.IndirectObjectEntry) []*chunkGroup {
	byID := map[object.ID]*chunkGroup{}

	var groups []*chunkGroup

	for _, c := range chunks {
		g := byID[c.Object]
		if g == nil {
			g = &chunkGroup{object: c.Object, length: c.Length}
			byID[c.Object] = g
			groups = append(groups, g)
		}

		g.offsets = append(g.offsets, c.Start)
	}

	return groups
}

type objectWriter struct {
	rep  repo.Repository
	w    io.WriterAt
	base int64
	opt  Options

	done         atomic.Int64
	fetchedBytes atomic.Int64

	mu    sync.Mutex
	zeros []image.Extent // object ranges of zero chunks
}

func (o *objectWriter) writeGroups(ctx context.Context, groups []*chunkGroup) error {
	eg, ctx := errgroup.WithContext(ctx)
	work := make(chan *chunkGroup)

	eg.Go(func() error {
		defer close(work)

		for _, g := range groups {
			select {
			case work <- g:
			case <-ctx.Done():
				return ctx.Err()
			}
		}

		return nil
	})

	for range o.opt.Parallel {
		eg.Go(func() error {
			var buf []byte

			for g := range work {
				if int64(len(buf)) < g.length {
					buf = o.opt.NewBuffer(int(g.length))
				}

				if err := o.writeGroup(ctx, g, buf[:g.length]); err != nil {
					return err
				}
			}

			return nil
		})
	}

	return errors.Wrap(eg.Wait(), "error restoring chunks")
}

func (o *objectWriter) writeGroup(ctx context.Context, g *chunkGroup, buf []byte) error {
	r, err := o.rep.OpenObject(ctx, g.object)
	if err != nil {
		return errors.Wrapf(err, "unable to open chunk %v", g.object)
	}

	_, err = io.ReadFull(r, buf)
	if err = stderrors.Join(err, r.Close()); err != nil {
		return errors.Wrapf(err, "unable to read chunk %v", g.object)
	}

	o.fetchedBytes.Add(g.length)

	if isZero(buf) {
		o.mu.Lock()
		for _, off := range g.offsets {
			if n := min(g.length, o.opt.Limit-off); n > 0 {
				o.zeros = append(o.zeros, image.Extent{Start: off, Length: n})
			}

			// zeros past the limit are left out
			if n := g.length - max(0, min(g.length, o.opt.Limit-off)); n > 0 {
				o.opt.Progress(o.done.Add(n))
			}
		}
		o.mu.Unlock()

		return nil
	}

	for _, off := range g.offsets {
		n := min(g.length, o.opt.Limit-off)
		cut, dataEnd := max(0, n), max(0, min(g.length, o.opt.DataEnd-off))

		if cut < dataEnd && !isZero(buf[cut:dataEnd]) {
			return errors.Errorf("the chunk at offset %v holds data past %v, where the object is cut", off, o.opt.Limit)
		}

		if n > 0 {
			if _, err := o.w.WriteAt(buf[:n], o.base+off); err != nil {
				return errors.Wrapf(err, "unable to write at offset %v", o.base+off)
			}
		}

		o.opt.Progress(o.done.Add(g.length))
	}

	return nil
}

// zeroRanges zeroes the merged ranges of the zero chunks in parallel pieces,
// unless the target already reads as zeros.
func (o *objectWriter) zeroRanges(ctx context.Context) error {
	merged, err := image.NormalizeExtents(o.zeros)
	if err != nil {
		return errors.Wrap(err, "invalid zero ranges")
	}

	o.zeros = merged

	if o.opt.TargetIsZeroed {
		o.opt.Progress(o.done.Add(image.TotalLength(merged)))
		return nil
	}

	var pieces []image.Extent

	for _, e := range merged {
		for off := e.Start; off < e.End(); off += zeroRangePieceSize {
			pieces = append(pieces, image.Extent{Start: off, Length: min(zeroRangePieceSize, e.End()-off)})
		}
	}

	eg, ctx := errgroup.WithContext(ctx)
	eg.SetLimit(o.opt.Parallel)

	for _, p := range pieces {
		eg.Go(func() error {
			if err := ctx.Err(); err != nil {
				return err //nolint:wrapcheck
			}

			if err := o.zero(o.base+p.Start, p.Length); err != nil {
				return errors.Wrapf(err, "unable to zero %v bytes at offset %v", p.Length, o.base+p.Start)
			}

			o.opt.Progress(o.done.Add(p.Length))

			return nil
		})
	}

	return errors.Wrap(eg.Wait(), "error zeroing free space")
}

func (o *objectWriter) zero(off, length int64) error {
	if o.opt.Zero != nil {
		return o.opt.Zero(off, length)
	}

	buf := o.opt.NewBuffer(int(min(length, zeroWriteSize)))
	clear(buf)

	for done := int64(0); done < length; {
		n := min(int64(len(buf)), length-done)

		if _, err := o.w.WriteAt(buf[:n], off+done); err != nil {
			return errors.Wrap(err, "write")
		}

		done += n
	}

	return nil
}

var zeroChunk = make([]byte, image.DefaultChunkSize)

func isZero(b []byte) bool {
	for len(b) > 0 {
		n := min(len(b), len(zeroChunk))
		if !bytes.Equal(b[:n], zeroChunk[:n]) {
			return false
		}

		b = b[n:]
	}

	return true
}

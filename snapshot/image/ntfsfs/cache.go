package ntfsfs

import (
	"container/list"
	"context"
	"io"
	"sort"
	"sync"
	"time"

	"github.com/pkg/errors"

	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/object"
	"github.com/kopia/kopia/snapshot/image"
	"github.com/kopia/kopia/snapshot/image/imagerestore"
)

// defaultCacheBytes is how much decoded image data an open volume keeps in
// memory by default, enough to keep $MFT and directory index blocks of a
// typical browsing session: 64 chunks of 4 MiB, 256 of 1 MiB.
const defaultCacheBytes = 256 << 20

// Read-ahead: once reads run on, as when a file is copied, the chunks that
// follow are fetched before they are asked for, each a round trip to a
// repository server. readAheadBytes is how far ahead, at most half the cache,
// readAheadFetches how many such fetches run at once, and readAheadStreams
// how many files read at the same time are followed.
const (
	readAheadBytes   = 32 << 20
	readAheadFetches = 16
	readAheadStreams = 8
)

// chunkFetchTimeout bounds one chunk fetch. Reads have no context of their
// own, and the NTFS parser holds the volume lock while it reads, so a fetch
// that never returned would block every other request for the volume.
const chunkFetchTimeout = 2 * time.Minute

// chunkReader is an io.ReaderAt over an image object that fetches only the
// chunks a read touches and keeps the most recently used ones in memory. It
// caches by chunk object ID, so every zero chunk of free space shares one
// cache entry. It is safe for concurrent use.
type chunkReader struct {
	ctx    context.Context //nolint:containedctx // io.ReaderAt has no context; the volume's lifetime bounds it
	rep    repo.Repository
	chunks []object.IndirectObjectEntry
	size   int64

	mu sync.Mutex
	// +checklocks:mu
	lru *list.List // of *cachedChunk, most recent first
	// +checklocks:mu
	byID map[object.ID]*list.Element
	// +checklocks:mu
	inflight map[object.ID]*fetch
	capacity int // +checklocksignore: set at creation

	fetches int64 // +checklocks:mu

	// streams are the recent runs of reads, each continuing where the last
	// one ended.
	// +checklocks:mu
	streams [readAheadStreams]readStream
	// +checklocks:mu
	nextStream int

	readAheadChunks int           // +checklocksignore: set at creation
	readAheadSlots  chan struct{} // +checklocksignore: set at creation
}

// readStream is a run of reads, each continuing where the last one ended.
type readStream struct {
	next       int64 // where the next read of the run starts
	runBytes   int64 // how much the run read
	prefetched int   // the last chunk fetched ahead for it, or -1
}

type cachedChunk struct {
	id   object.ID
	data []byte
}

type fetch struct {
	done chan struct{}
	data []byte
	err  error
}

func newChunkReader(ctx context.Context, rep repo.Repository, oid object.ID, capacity int) (*chunkReader, error) {
	chunks, size, err := imagerestore.Chunks(ctx, rep, oid)
	if err != nil {
		return nil, errors.Wrap(err, "unable to read the image chunk list")
	}

	chunkLen := int64(image.DefaultChunkSize)
	if len(chunks) > 0 {
		chunkLen = chunks[0].Length
	}

	if capacity <= 0 {
		capacity = int(max(1, defaultCacheBytes/chunkLen))
	}

	c := &chunkReader{
		ctx:             ctx,
		rep:             rep,
		chunks:          chunks,
		size:            size,
		lru:             list.New(),
		byID:            map[object.ID]*list.Element{},
		inflight:        map[object.ID]*fetch{},
		capacity:        capacity,
		readAheadChunks: int(min(readAheadBytes/chunkLen, int64(capacity/2))), //nolint:mnd
		readAheadSlots:  make(chan struct{}, readAheadFetches),
	}

	for i := range c.streams {
		c.streams[i].prefetched = -1
	}

	return c, nil
}

// Size returns the image size in bytes.
func (c *chunkReader) Size() int64 {
	return c.size
}

// Fetches returns the number of chunks fetched from the repository.
func (c *chunkReader) Fetches() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.fetches
}

// ReadAt implements io.ReaderAt.
func (c *chunkReader) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.Errorf("invalid offset %v", off)
	}

	if off >= c.size {
		return 0, io.EOF
	}

	end := min(off+int64(len(p)), c.size)

	// the first chunk that ends after off
	i := sort.Search(len(c.chunks), func(i int) bool { return c.chunks[i].Start+c.chunks[i].Length > off })

	c.readAhead(off, end)

	n := 0

	for pos := off; pos < end; i++ {
		ch := c.chunks[i]

		data, err := c.chunk(ch)
		if err != nil {
			return n, err
		}

		copied := copy(p[pos-off:end-off], data[pos-ch.Start:])
		n += copied
		pos += int64(copied)
	}

	if n < len(p) {
		return n, io.EOF
	}

	return n, nil
}

// chunk returns a chunk's data, fetching it once even when several readers
// ask for it at the same time.
func (c *chunkReader) chunk(ch object.IndirectObjectEntry) ([]byte, error) {
	c.mu.Lock()

	if el, ok := c.byID[ch.Object]; ok {
		c.lru.MoveToFront(el)
		data := el.Value.(*cachedChunk).data //nolint:forcetypeassert
		c.mu.Unlock()

		return data, nil
	}

	if f, ok := c.inflight[ch.Object]; ok {
		c.mu.Unlock()
		<-f.done

		return f.data, f.err
	}

	f := &fetch{done: make(chan struct{})}
	c.inflight[ch.Object] = f
	c.fetches++
	c.mu.Unlock()

	f.data, f.err = c.fetch(ch)

	c.mu.Lock()
	delete(c.inflight, ch.Object)

	if f.err == nil {
		c.byID[ch.Object] = c.lru.PushFront(&cachedChunk{id: ch.Object, data: f.data})

		for c.lru.Len() > c.capacity {
			oldest := c.lru.Back()
			c.lru.Remove(oldest)
			delete(c.byID, oldest.Value.(*cachedChunk).id) //nolint:forcetypeassert
		}
	}

	c.mu.Unlock()
	close(f.done)

	return f.data, f.err
}

// readAhead follows the runs of reads and, once a run has read a chunk's
// worth, fetches the chunks after the read in the background: a file copied
// from a mounted image is read in order, chunk after chunk.
func (c *chunkReader) readAhead(off, end int64) {
	if c.readAheadChunks <= 0 {
		return
	}

	c.mu.Lock()

	s := c.streamAt(off)
	s.next = end
	s.runBytes += end - off

	// the chunk holding the last byte read
	last := sort.Search(len(c.chunks), func(i int) bool { return c.chunks[i].Start+c.chunks[i].Length >= end })

	var ahead []object.IndirectObjectEntry

	if last < len(c.chunks) && s.runBytes >= c.chunks[last].Length {
		from := max(last+1, s.prefetched+1)
		to := min(last+c.readAheadChunks, len(c.chunks)-1)

		scheduled := map[object.ID]bool{}

		for k := from; k <= to; k++ {
			ch := c.chunks[k]

			_, cached := c.byID[ch.Object]
			if !cached && c.inflight[ch.Object] == nil && !scheduled[ch.Object] {
				// with a full set of fetches ahead under way, the rest is
				// fetched ahead on a later read
				if !c.takeReadAheadSlot() {
					break
				}

				scheduled[ch.Object] = true
				ahead = append(ahead, ch)
			}

			s.prefetched = k
		}
	}

	c.mu.Unlock()

	for _, ch := range ahead {
		go func() {
			defer func() { <-c.readAheadSlots }()

			// a failed fetch is fetched again when the read gets there
			c.chunk(ch) //nolint:errcheck
		}()
	}
}

// takeReadAheadSlot reserves one of the fetches ahead that may run at once,
// without waiting.
func (c *chunkReader) takeReadAheadSlot() bool {
	select {
	case c.readAheadSlots <- struct{}{}:
		return true
	default:
		return false
	}
}

// streamAt returns the run of reads that a read at off continues, or starts
// a new one in place of the oldest.
//
// +checklocks:c.mu
func (c *chunkReader) streamAt(off int64) *readStream {
	for i := range c.streams {
		if s := &c.streams[i]; s.runBytes > 0 && s.next == off {
			return s
		}
	}

	s := &c.streams[c.nextStream]
	c.nextStream = (c.nextStream + 1) % len(c.streams)
	*s = readStream{next: off, prefetched: -1}

	return s
}

// purge drops the cached chunks.
func (c *chunkReader) purge() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.lru.Init()
	clear(c.byID)
}

func (c *chunkReader) fetch(ch object.IndirectObjectEntry) ([]byte, error) {
	ctx, cancel := context.WithTimeout(c.ctx, chunkFetchTimeout)
	defer cancel()

	r, err := c.rep.OpenObject(ctx, ch.Object)
	if err != nil {
		return nil, errors.Wrapf(err, "unable to open image chunk at offset %v", ch.Start)
	}
	defer r.Close() //nolint:errcheck

	data := make([]byte, ch.Length)

	if _, err := io.ReadFull(r, data); err != nil {
		return nil, errors.Wrapf(err, "unable to read image chunk at offset %v", ch.Start)
	}

	return data, nil
}

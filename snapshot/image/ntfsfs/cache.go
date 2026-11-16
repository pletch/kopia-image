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
	"github.com/kopia/kopia/snapshot/image/imagerestore"
)

// DefaultCacheChunks is the default number of decoded 4 MiB chunks kept in
// memory per open volume (256 MiB), enough to keep $MFT and directory index
// blocks of a typical browsing session in memory.
const DefaultCacheChunks = 64

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

	if capacity <= 0 {
		capacity = DefaultCacheChunks
	}

	return &chunkReader{
		ctx:      ctx,
		rep:      rep,
		chunks:   chunks,
		size:     size,
		lru:      list.New(),
		byID:     map[object.ID]*list.Element{},
		inflight: map[object.ID]*fetch{},
		capacity: capacity,
	}, nil
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

package ntfsfs

import (
	"container/list"
	"sync"

	"github.com/kopia/kopia/fs"
)

// maxCachedDirEntries bounds the directory cache by the number of entries it
// holds, a few hundred bytes each: enough for C:\Windows\WinSxS and the
// folders around it.
const maxCachedDirEntries = 250_000

// dirListing is the entries of a directory with an index by name.
type dirListing struct {
	entries []fs.Entry
	byName  map[string]fs.Entry
}

func newDirListing(entries []fs.Entry) *dirListing {
	l := &dirListing{
		entries: entries,
		byName:  make(map[string]fs.Entry, len(entries)),
	}

	for _, e := range entries {
		if _, ok := l.byName[e.Name()]; !ok {
			l.byName[e.Name()] = e
		}
	}

	return l
}

// dirCache keeps the most recently used directory listings of a volume. An
// image never changes, so a listing never goes stale. It is safe for
// concurrent use.
type dirCache struct {
	mu sync.Mutex
	// +checklocks:mu
	lru *list.List // of *cachedDir, most recent first
	// +checklocks:mu
	byMFT map[int64]*list.Element
	// +checklocks:mu
	entries int
}

type cachedDir struct {
	mft     int64
	listing *dirListing
}

func (c *dirCache) get(mft int64) *dirListing {
	c.mu.Lock()
	defer c.mu.Unlock()

	el, ok := c.byMFT[mft]
	if !ok {
		return nil
	}

	c.lru.MoveToFront(el)

	return el.Value.(*cachedDir).listing //nolint:forcetypeassert
}

func (c *dirCache) put(mft int64, l *dirListing) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.lru == nil {
		c.lru = list.New()
		c.byMFT = map[int64]*list.Element{}
	}

	if _, ok := c.byMFT[mft]; ok {
		return
	}

	c.byMFT[mft] = c.lru.PushFront(&cachedDir{mft: mft, listing: l})
	c.entries += len(l.entries)

	// keep at least the listing just added, however large
	for c.entries > maxCachedDirEntries && c.lru.Len() > 1 {
		oldest := c.lru.Remove(c.lru.Back()).(*cachedDir) //nolint:forcetypeassert
		delete(c.byMFT, oldest.mft)
		c.entries -= len(oldest.listing.entries)
	}
}

func (c *dirCache) purge() {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.lru != nil {
		c.lru.Init()
		clear(c.byMFT)
	}

	c.entries = 0
}

//go:build windows && !cgo

package winfspmount

import (
	"container/list"
	"context"
	"strings"
	"sync"

	"github.com/pkg/errors"
	"golang.org/x/sync/singleflight"

	"github.com/kopia/kopia/fs"
)

// defaultMaxCachedEntries bounds the directory cache by the number of entries
// it holds, a few hundred bytes each.
const defaultMaxCachedEntries = 250_000

// dirListing is what a directory holds that a drive can show: files and
// directories, indexed by name.
type dirListing struct {
	entries []fs.Entry
	byName  map[string]fs.Entry
	byFold  map[string]fs.Entry // by lower-case name
}

func newDirListing(all []fs.Entry) *dirListing {
	l := &dirListing{
		byName: make(map[string]fs.Entry, len(all)),
		byFold: make(map[string]fs.Entry, len(all)),
	}

	for _, e := range all {
		switch e.(type) {
		case fs.Directory, fs.File:
		default:
			// symlinks and other special entries cannot be shown
			continue
		}

		if _, ok := l.byName[e.Name()]; ok {
			continue
		}

		l.entries = append(l.entries, e)
		l.byName[e.Name()] = e

		if folded := strings.ToLower(e.Name()); l.byFold[folded] == nil {
			l.byFold[folded] = e
		}
	}

	return l
}

// find returns the entry with the given name, preferring an exact match to
// one that differs in case.
func (l *dirListing) find(name string) fs.Entry {
	if e, ok := l.byName[name]; ok {
		return e
	}

	return l.byFold[strings.ToLower(name)]
}

// dirCache keeps the most recently used directory listings, keyed by path. A
// snapshot never changes, so a listing never goes stale.
type dirCache struct {
	ctx        context.Context //nolint:containedctx // listings are read with the mount's context
	maxEntries int
	loads      singleflight.Group

	mu sync.Mutex
	// +checklocks:mu
	lru *list.List // of *cachedDir, most recent first
	// +checklocks:mu
	byPath map[string]*list.Element
	// +checklocks:mu
	entries int
}

type cachedDir struct {
	path    string
	listing *dirListing
}

func newDirCache(ctx context.Context, maxEntries int) *dirCache {
	return &dirCache{
		ctx:        ctx,
		maxEntries: maxEntries,
		lru:        list.New(),
		byPath:     map[string]*list.Element{},
	}
}

// get returns the listing of the directory at path, listing it once when
// several threads ask for it at the same time.
func (c *dirCache) get(path string, dir fs.Directory) (*dirListing, error) {
	if l := c.lookup(path); l != nil {
		return l, nil
	}

	v, err, _ := c.loads.Do(path, func() (any, error) {
		if l := c.lookup(path); l != nil {
			return l, nil
		}

		all, err := fs.GetAllEntries(c.ctx, dir)
		if err != nil {
			return nil, errors.Wrap(err, "list directory")
		}

		l := newDirListing(all)
		c.put(path, l)

		return l, nil
	})
	if err != nil {
		return nil, err //nolint:wrapcheck
	}

	return v.(*dirListing), nil //nolint:forcetypeassert
}

func (c *dirCache) lookup(path string) *dirListing {
	c.mu.Lock()
	defer c.mu.Unlock()

	el, ok := c.byPath[path]
	if !ok {
		return nil
	}

	c.lru.MoveToFront(el)

	return el.Value.(*cachedDir).listing //nolint:forcetypeassert
}

func (c *dirCache) put(path string, l *dirListing) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if _, ok := c.byPath[path]; ok {
		return
	}

	c.byPath[path] = c.lru.PushFront(&cachedDir{path: path, listing: l})
	c.entries += len(l.entries)

	// keep at least the listing just added, however large
	for c.entries > c.maxEntries && c.lru.Len() > 1 {
		oldest := c.lru.Remove(c.lru.Back()).(*cachedDir) //nolint:forcetypeassert
		delete(c.byPath, oldest.path)
		c.entries -= len(oldest.listing.entries)
	}
}

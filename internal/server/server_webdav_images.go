package server

import (
	"context"
	"slices"
	"sync"

	"github.com/kopia/kopia/fs"
	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/object"
	"github.com/kopia/kopia/snapshot/image/ntfsfs"
	"github.com/kopia/kopia/snapshot/snapshotfs"
)

// maxCachedImageRoots bounds the image snapshots whose NTFS volumes stay
// open between WebDAV requests; each open volume caches up to 256 MiB of
// image chunks, which are released when its snapshot leaves the cache.
const maxCachedImageRoots = 4

// imageRootCache keeps the browsable roots of recently served image
// snapshots, because the WebDAV handler builds its tree on every request and
// reopening an NTFS volume each time would re-parse it and discard its chunk
// cache.
type imageRootCache struct {
	mu sync.Mutex
	// +checklocks:mu
	rep repo.Repository
	// +checklocks:mu
	roots map[object.ID]fs.Directory
	// +checklocks:mu
	order []object.ID // least recently used first
}

// webdavSnapshotRoot returns the WebDAV tree of a snapshot root: for an image
// snapshot, with a folder of files for each NTFS volume.
func (s *Server) webdavSnapshotRoot(ctx context.Context, rep repo.Repository, oid object.ID) fs.Directory {
	root, evicted := s.webdavImageRoots.get(ctx, rep, oid)

	// Releasing waits for the requests that are reading the volume, each of
	// which may be fetching from the repository, so it must not hold up this
	// request or, under the cache's lock, every other one.
	if len(evicted) > 0 {
		go func() {
			for _, e := range evicted {
				ntfsfs.ReleaseVolumes(e)
			}
		}()
	}

	return root
}

// get returns the cached root of a snapshot, opening it if needed, and the
// roots that left the cache, whose volumes the caller must release.
func (c *imageRootCache) get(ctx context.Context, rep repo.Repository, oid object.ID) (root fs.Directory, evicted []fs.Directory) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// a reconnected repository invalidates the open volumes
	if c.rep != rep {
		for _, r := range c.roots {
			evicted = append(evicted, r)
		}

		c.rep, c.roots, c.order = rep, map[object.ID]fs.Directory{}, nil
	}

	if cached, ok := c.roots[oid]; ok {
		c.order = append(slices.DeleteFunc(c.order, func(o object.ID) bool { return o == oid }), oid)
		return cached, evicted
	}

	dir := snapshotfs.DirectoryEntry(rep, oid, nil)

	root = ntfsfs.WithBrowsableVolumes(ctx, rep, dir)
	if root == dir {
		// not an image snapshot; nothing worth keeping
		return dir, evicted
	}

	c.roots[oid] = root
	c.order = append(c.order, oid)

	// a request still using an evicted root keeps working: its volumes are
	// reopened on demand
	for len(c.order) > maxCachedImageRoots {
		evicted = append(evicted, c.roots[c.order[0]])
		delete(c.roots, c.order[0])
		c.order = c.order[1:]
	}

	return root, evicted
}

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
// image chunks.
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
	c := &s.webdavImageRoots

	c.mu.Lock()
	defer c.mu.Unlock()

	// a reconnected repository invalidates the open volumes
	if c.rep != rep {
		c.rep, c.roots, c.order = rep, map[object.ID]fs.Directory{}, nil
	}

	if root, ok := c.roots[oid]; ok {
		c.order = append(slices.DeleteFunc(c.order, func(o object.ID) bool { return o == oid }), oid)
		return root
	}

	dir := snapshotfs.DirectoryEntry(rep, oid, nil)

	root := ntfsfs.WithBrowsableVolumes(ctx, rep, dir)
	if root == dir {
		// not an image snapshot; nothing worth keeping
		return dir
	}

	c.roots[oid] = root
	c.order = append(c.order, oid)

	for len(c.order) > maxCachedImageRoots {
		delete(c.roots, c.order[0])
		c.order = c.order[1:]
	}

	return root
}

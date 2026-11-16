package server

import (
	"context"
	"maps"

	"github.com/pkg/errors"

	"github.com/kopia/kopia/internal/mount"
	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/object"
	"github.com/kopia/kopia/snapshot/image/ntfsfs"
	"github.com/kopia/kopia/snapshot/snapshotfs"
)

// getMountController returns the mount controller of the given snapshot directory, optionally creating it.
// With WebDAV mounts enabled, a new controller is a WebDAV share at webdavURL; otherwise the directory
// is mounted on the server host.
func (s *Server) getMountController(ctx context.Context, rep repo.Repository, oid object.ID, createIfNotFound bool, webdavURL string) (mount.Controller, error) {
	s.serverMutex.Lock()
	defer s.serverMutex.Unlock()

	c := s.mounts[oid]
	if c != nil {
		return c, nil
	}

	if !createIfNotFound {
		return nil, nil
	}

	if s.options.WebDAVMounts {
		c = newWebDAVShare(webdavURL)
		s.mounts[oid] = c

		return c, nil
	}

	userLog(ctx).Debugf("mount controller for %v not found, starting", oid)

	// an image snapshot shows each NTFS volume's files next to its image
	root := ntfsfs.WithBrowsableVolumes(ctx, rep, snapshotfs.DirectoryEntry(rep, oid, nil))

	c, err := mount.Directory(ctx, root, "*", mount.Options{
		DriveLabel: mount.SnapshotDriveLabel(ctx, rep, oid),
	})
	if err != nil {
		return nil, errors.Wrap(err, "unable to mount")
	}

	s.mounts[oid] = c

	return c, nil
}

func (s *Server) listMounts() map[object.ID]mount.Controller {
	s.serverMutex.RLock()
	defer s.serverMutex.RUnlock()

	return maps.Clone(s.mounts)
}

func (s *Server) deleteMount(oid object.ID) {
	s.serverMutex.Lock()
	defer s.serverMutex.Unlock()

	delete(s.mounts, oid)
}

// +checklocks:s.serverMutex
func (s *Server) unmountAllLocked(ctx context.Context) {
	for oid, c := range s.mounts {
		if err := c.Unmount(ctx); err != nil {
			userLog(ctx).Errorf("unable to unmount %v", oid)
		}

		delete(s.mounts, oid)
	}
}

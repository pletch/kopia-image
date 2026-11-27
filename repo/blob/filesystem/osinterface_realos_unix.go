//go:build linux || freebsd || darwin

package filesystem

import (
	"os"
	"syscall"

	"github.com/pkg/errors"
)

func (realOS) IsStale(err error) bool {
	return errors.Is(err, syscall.ESTALE)
}

func (realOS) SyncDir(dirname string) error {
	d, err := os.Open(dirname) //nolint:gosec
	if err != nil {
		return errors.Wrap(err, "open directory")
	}

	defer d.Close() //nolint:errcheck

	// some file systems, such as certain network mounts, can't sync a directory.
	if err := d.Sync(); err != nil && !errors.Is(err, syscall.EINVAL) && !errors.Is(err, syscall.ENOTSUP) {
		return errors.Wrap(err, "sync directory")
	}

	return nil
}

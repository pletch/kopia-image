//go:build windows && cgo

package winfspmount

import (
	"context"

	"github.com/pkg/errors"

	"github.com/kopia/kopia/fs"
)

// ErrNotInstalled is returned by Mount when WinFsp cannot be used.
var ErrNotInstalled = errors.New("WinFsp mounts are not supported by builds with cgo")

// Controller is a mounted directory.
type Controller struct{}

// Available reports whether WinFsp can be used: never with cgo.
func Available() bool { return false }

// Mount fails: builds with cgo do not support WinFsp.
func Mount(context.Context, fs.Directory, string, string) (*Controller, error) {
	return nil, ErrNotInstalled
}

// MountPath returns the drive.
func (c *Controller) MountPath() string { return "" }

// Done is closed when the drive has been unmounted.
func (c *Controller) Done() <-chan struct{} { return nil }

// Unmount removes the drive.
func (c *Controller) Unmount(context.Context) error { return ErrNotInstalled }

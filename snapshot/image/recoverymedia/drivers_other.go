//go:build !windows

package recoverymedia

import (
	"context"

	"github.com/kopia/kopia/snapshot/image"
)

// HostDrivers is supported only on Windows.
func HostDrivers(context.Context) ([]Driver, error) { return nil, ErrNotSupported }

// ExportHostDrivers is supported only on Windows.
func ExportHostDrivers(context.Context, string) ([]image.DriverSource, []string, error) {
	return nil, nil, ErrNotSupported
}

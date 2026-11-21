//go:build !linux && !windows

package imagerestore

import (
	"context"

	"github.com/pkg/errors"
)

func openDevice(path string, _ deviceOptions) (Target, error) {
	return nil, errors.Errorf("restoring to block devices is supported on Linux and Windows; %v is not a regular file", path)
}

func otherDiskGUIDs(Target) (map[string]string, error) {
	return nil, nil
}

// ListDisks is supported on Linux and Windows.
func ListDisks() ([]DiskInfo, error) {
	return nil, errors.New("listing disks is supported on Linux and Windows")
}

// CleanDisk is supported on Linux and Windows.
func CleanDisk(context.Context, string) error {
	return errors.New("cleaning disks is supported on Linux and Windows")
}

// InRecoveryEnvironment reports whether this is a Windows recovery system.
func InRecoveryEnvironment() bool {
	return false
}

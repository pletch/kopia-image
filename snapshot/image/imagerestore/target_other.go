//go:build !linux && !windows

package imagerestore

import "github.com/pkg/errors"

func openDevice(path string, _ bool) (Target, error) {
	return nil, errors.Errorf("restoring to block devices is supported on Linux and Windows; %v is not a regular file", path)
}

func otherDiskGUIDs(Target) (map[string]string, error) {
	return nil, nil
}

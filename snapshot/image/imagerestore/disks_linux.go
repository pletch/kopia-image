package imagerestore

import (
	"context"
	"os"
	"path/filepath"
)

// ListDisks returns the whole disks of this computer, without loop, RAM and
// device-mapper devices, with what uses them; see scanSysDisks.
func ListDisks() ([]DiskInfo, error) {
	disks, err := scanSysDisks(os.DirFS("/"), resolveDevicePath)
	if err != nil {
		return nil, err
	}

	for i := range disks {
		if guid, ok := readDiskGUID(disks[i].Path); ok {
			disks[i].DiskGUID = guid
		}
	}

	return disks, nil
}

// resolveDevicePath follows the symbolic links of a device path, such as
// /dev/mapper/vg-data to /dev/dm-0, and returns it as it is if it cannot.
func resolveDevicePath(dev string) string {
	if r, err := filepath.EvalSymlinks(dev); err == nil {
		return r
	}

	return dev
}

// CleanDisk is not needed on Linux: a disk whose partitions are not mounted
// is opened and overwritten as it is.
func CleanDisk(context.Context, string) error {
	return nil
}

// InRecoveryEnvironment reports whether this is a Windows recovery system.
func InRecoveryEnvironment() bool {
	return false
}

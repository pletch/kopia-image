package imagerestore

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	pkgerrors "github.com/pkg/errors"
)

const (
	sysBlock           = "/sys/block"
	sysBlockSectorSize = 512 // the unit of /sys/block/*/size, whatever the device's sector size
)

// ListDisks returns the whole disks of this computer, without loop, RAM and
// device-mapper devices.
func ListDisks() ([]DiskInfo, error) {
	entries, err := os.ReadDir(sysBlock)
	if err != nil {
		return nil, pkgerrors.Wrap(err, "unable to list block devices")
	}

	mounts := readMounts()

	var out []DiskInfo

	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, "loop") || strings.HasPrefix(name, "ram") || strings.HasPrefix(name, "dm-") || strings.HasPrefix(name, "zram") || strings.HasPrefix(name, "sr") {
			continue
		}

		dir := filepath.Join(sysBlock, name)

		sectors, err := strconv.ParseInt(readSysString(filepath.Join(dir, "size")), 10, 64)
		if err != nil || sectors == 0 {
			continue
		}

		sectorSize, err := strconv.ParseInt(readSysString(filepath.Join(dir, "queue", "logical_block_size")), 10, 64)
		if err != nil {
			continue
		}

		d := DiskInfo{
			Path:       "/dev/" + name,
			Model:      strings.TrimSpace(readSysString(filepath.Join(dir, "device", "vendor")) + " " + readSysString(filepath.Join(dir, "device", "model"))),
			Removable:  readSysString(filepath.Join(dir, "removable")) == "1",
			Size:       sectors * sysBlockSectorSize,
			SectorSize: sectorSize,
		}

		for dev, mountPoints := range mounts {
			if dev == d.Path || strings.HasPrefix(dev, d.Path) {
				d.Volumes = append(d.Volumes, DiskVolume{FileSystem: dev, MountPoints: mountPoints})
			}
		}

		out = append(out, d)
	}

	return out, nil
}

func readSysString(path string) string {
	b, err := os.ReadFile(path) //nolint:gosec
	if err != nil {
		return ""
	}

	return strings.TrimSpace(string(b))
}

// readMounts returns the mount points of each mounted device.
func readMounts() map[string][]string {
	b, err := os.ReadFile("/proc/mounts")
	if err != nil {
		return nil
	}

	out := map[string][]string{}

	for line := range strings.SplitSeq(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && strings.HasPrefix(f[0], "/dev/") {
			out[f[0]] = append(out[f[0]], f[1])
		}
	}

	return out
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

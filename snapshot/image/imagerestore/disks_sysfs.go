package imagerestore

import (
	"io/fs"
	"path"
	"slices"
	"strconv"
	"strings"

	"github.com/pkg/errors"
)

// sysBlockSectorSize is the unit of /sys/block/*/size, whatever the device's
// sector size.
const sysBlockSectorSize = 512

// mountEntry is a line of /proc/mounts for a device under /dev.
type mountEntry struct {
	dev   string // as mounted, such as /dev/mapper/vg-data
	kname string // the kernel name of the device, such as dm-0
	dir   string
}

// scanSysDisks lists the whole disks in sysfs, without loop, RAM, optical
// and device-mapper devices, with what uses them. fsys is the root of the
// file system ("sys/block/...", "proc/mounts"), and resolve turns a device
// path from /proc/mounts or /proc/swaps, such as /dev/mapper/vg-data, into
// the device node it names, such as /dev/dm-0.
//
// A disk is in use when it, or one of its partitions, is mounted, is swap,
// is held by another block device (device mapper, LVM, RAID, encryption)
// or is a member of a mounted multi-device btrfs file system. The volumes of
// a disk include the file systems mounted from devices stacked on it and
// those of the btrfs file systems it is a member of, so that a disk holding
// part of a btrfs repository is recognized as holding it.
func scanSysDisks(fsys fs.FS, resolve func(string) string) ([]DiskInfo, error) {
	entries, err := fs.ReadDir(fsys, "sys/block")
	if err != nil {
		return nil, errors.Wrap(err, "unable to list block devices")
	}

	kname := func(dev string) string { return path.Base(resolve(dev)) }

	mounts := readMountEntries(fsys, kname)
	swaps := readSwaps(fsys, kname)
	btrfs := readBtrfsMembers(fsys)

	var out []DiskInfo

	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, "loop") || strings.HasPrefix(name, "ram") || strings.HasPrefix(name, "dm-") || strings.HasPrefix(name, "zram") || strings.HasPrefix(name, "sr") {
			continue
		}

		dir := path.Join("sys/block", name)

		sectors, err := strconv.ParseInt(readSysFile(fsys, path.Join(dir, "size")), 10, 64)
		if err != nil || sectors == 0 {
			continue
		}

		sectorSize, err := strconv.ParseInt(readSysFile(fsys, path.Join(dir, "queue", "logical_block_size")), 10, 64)
		if err != nil {
			continue
		}

		d := DiskInfo{
			Path:       "/dev/" + name,
			Model:      strings.TrimSpace(readSysFile(fsys, path.Join(dir, "device", "vendor")) + " " + readSysFile(fsys, path.Join(dir, "device", "model"))),
			Removable:  readSysFile(fsys, path.Join(dir, "removable")) == "1",
			Size:       sectors * sysBlockSectorSize,
			SectorSize: sectorSize,
		}

		describeDiskUse(fsys, &d, name, mounts, swaps, btrfs)

		out = append(out, d)
	}

	return out, nil
}

// describeDiskUse sets the volumes of a disk and whether it is in use.
func describeDiskUse(fsys fs.FS, d *DiskInfo, name string, mounts []mountEntry, swaps []string, btrfs [][]string) {
	parts := sysPartitions(fsys, name)

	// the disk, its partitions and the devices stacked on them
	stack := append([]string{name}, parts...)
	holders := sysHolders(fsys, name, parts)
	stack = append(stack, holders...)

	var volumes []DiskVolume

	addMount := func(fileSystem string, m mountEntry) {
		i := slices.IndexFunc(volumes, func(v DiskVolume) bool { return v.FileSystem == fileSystem })
		if i < 0 {
			volumes = append(volumes, DiskVolume{FileSystem: fileSystem})
			i = len(volumes) - 1
		}

		if !slices.Contains(volumes[i].MountPoints, m.dir) {
			volumes[i].MountPoints = append(volumes[i].MountPoints, m.dir)
		}
	}

	for _, m := range mounts {
		if slices.Contains(stack, m.kname) {
			addMount(m.dev, m)
			continue
		}

		// a btrfs file system mounted from another of its devices
		for _, members := range btrfs {
			if !slices.Contains(members, m.kname) {
				continue
			}

			for _, k := range stack {
				if slices.Contains(members, k) {
					addMount("/dev/"+k+" (btrfs, with "+m.dev+")", m)
				}
			}
		}
	}

	for _, k := range stack {
		if slices.Contains(swaps, k) {
			volumes = append(volumes, DiskVolume{FileSystem: "/dev/" + k + " (swap)"})
		}
	}

	if len(volumes) == 0 && len(holders) > 0 {
		names := make([]string, len(holders))
		for i, h := range holders {
			names[i] = "/dev/" + h
		}

		volumes = append(volumes, DiskVolume{FileSystem: "used by " + strings.Join(names, ", ")})
	}

	d.Volumes = volumes
	d.InUse = len(volumes) > 0
}

// sysPartitions returns the kernel names of a disk's partitions.
func sysPartitions(fsys fs.FS, disk string) []string {
	entries, err := fs.ReadDir(fsys, path.Join("sys/block", disk))
	if err != nil {
		return nil
	}

	var out []string

	for _, e := range entries {
		if _, err := fs.Stat(fsys, path.Join("sys/block", disk, e.Name(), "partition")); err == nil {
			out = append(out, e.Name())
		}
	}

	return out
}

// sysHolders returns the devices stacked on a disk or its partitions, such
// as dm-0 for an LVM volume or md0 for a RAID array, and those stacked on
// them in turn.
func sysHolders(fsys fs.FS, disk string, parts []string) []string {
	queue := []string{path.Join("sys/block", disk, "holders")}
	for _, p := range parts {
		queue = append(queue, path.Join("sys/block", disk, p, "holders"))
	}

	var out []string

	for len(queue) > 0 {
		dir := queue[0]
		queue = queue[1:]

		entries, err := fs.ReadDir(fsys, dir)
		if err != nil {
			continue
		}

		for _, e := range entries {
			h := e.Name()
			if slices.Contains(out, h) || h == disk {
				continue
			}

			out = append(out, h)
			queue = append(queue, path.Join("sys/block", h, "holders"))
		}
	}

	return out
}

// readMountEntries returns the mounts of devices under /dev, in the order of
// /proc/mounts.
func readMountEntries(fsys fs.FS, kname func(string) string) []mountEntry {
	b, err := fs.ReadFile(fsys, "proc/mounts")
	if err != nil {
		return nil
	}

	var out []mountEntry

	for line := range strings.SplitSeq(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && strings.HasPrefix(f[0], "/dev/") {
			out = append(out, mountEntry{dev: f[0], kname: kname(f[0]), dir: unescapeMountField(f[1])})
		}
	}

	return out
}

// unescapeMountField undoes the octal escapes of spaces, tabs, newlines and
// backslashes in /proc/mounts.
func unescapeMountField(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}

	var sb strings.Builder

	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				sb.WriteByte(byte(v))

				i += 3

				continue
			}
		}

		sb.WriteByte(s[i])
	}

	return sb.String()
}

// readSwaps returns the kernel names of the devices in /proc/swaps.
func readSwaps(fsys fs.FS, kname func(string) string) []string {
	b, err := fs.ReadFile(fsys, "proc/swaps")
	if err != nil {
		return nil
	}

	var out []string

	for line := range strings.SplitSeq(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) >= 1 && strings.HasPrefix(f[0], "/dev/") {
			out = append(out, kname(f[0]))
		}
	}

	return out
}

// readBtrfsMembers returns the kernel names of the devices of each mounted
// btrfs file system, from /sys/fs/btrfs/<uuid>/devices.
func readBtrfsMembers(fsys fs.FS) [][]string {
	entries, err := fs.ReadDir(fsys, "sys/fs/btrfs")
	if err != nil {
		return nil
	}

	var out [][]string

	for _, e := range entries {
		devices, err := fs.ReadDir(fsys, path.Join("sys/fs/btrfs", e.Name(), "devices"))
		if err != nil || len(devices) == 0 {
			continue
		}

		members := make([]string, len(devices))
		for i, d := range devices {
			members[i] = d.Name()
		}

		out = append(out, members)
	}

	return out
}

func readSysFile(fsys fs.FS, name string) string {
	b, err := fs.ReadFile(fsys, name)
	if err != nil {
		return ""
	}

	return strings.TrimSpace(string(b))
}

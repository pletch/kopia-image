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

// mountEntry is a line of /proc/self/mountinfo for a device under /dev.
type mountEntry struct {
	dev   string // as mounted, such as /dev/mapper/vg-data
	kname string // the kernel name of the device, such as dm-0
	dir   string
}

// scanSysDisks lists the whole disks in sysfs, without loop, RAM, optical
// and device-mapper devices, with what uses them. fsys is the root of the
// file system ("sys/block/...", "proc/self/mountinfo"), and resolve turns a
// device path from mountinfo or /proc/swaps, such as /dev/mapper/vg-data,
// into the device node it names, such as /dev/dm-0. A mount whose device
// path names no block device, such as /dev/root, is found by its device
// number instead. It fails if the mounts cannot be read, rather than show
// every disk as unused.
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

	numbers := sysDeviceNumbers(fsys, entries)

	known := map[string]bool{}
	for _, n := range numbers {
		known[n] = true
	}

	kname := func(dev, number string) string {
		k := path.Base(resolve(dev))
		if known[k] {
			return k
		}

		if n, ok := numbers[number]; ok {
			return n
		}

		return k
	}

	mounts, err := readMountEntries(fsys, kname)
	if err != nil {
		return nil, err
	}

	swaps, err := readSwaps(fsys, kname)
	if err != nil {
		return nil, err
	}

	btrfs := readBtrfsMembers(fsys)

	var out []DiskInfo

	for _, e := range entries {
		name := e.Name()
		if !isDiskName(name) {
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
			Serial:     sysDiskSerial(fsys, dir),
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

// sysDiskSerial returns the serial number of a disk, or its world-wide
// identifier when it reports no serial number.
func sysDiskSerial(fsys fs.FS, dir string) string {
	for _, f := range []string{"device/serial", "serial", "wwid", "device/wwid"} {
		if s := readSysFile(fsys, path.Join(dir, f)); s != "" {
			return s
		}
	}

	return ""
}

// isDiskName reports whether a block device name can be a disk: not a loop,
// RAM, device-mapper, optical or floppy device.
func isDiskName(name string) bool {
	for _, p := range []string{"loop", "ram", "dm-", "zram", "sr", "fd"} {
		if strings.HasPrefix(name, p) {
			return false
		}
	}

	return true
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
// as dm-0 for an LVM volume or md0 for a RAID array, their partitions, and
// the devices stacked on those in turn.
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

			// a partitioned RAID array: md0p1 and what is stacked on it
			for _, p := range sysPartitions(fsys, h) {
				if !slices.Contains(out, p) {
					out = append(out, p)
					queue = append(queue, path.Join("sys/block", h, p, "holders"))
				}
			}
		}
	}

	return out
}

// sysDeviceNumbers returns the kernel name of every block device and
// partition in sysfs by its device number ("8:1").
func sysDeviceNumbers(fsys fs.FS, entries []fs.DirEntry) map[string]string {
	out := map[string]string{}

	for _, e := range entries {
		if n := readSysFile(fsys, path.Join("sys/block", e.Name(), "dev")); n != "" {
			out[n] = e.Name()
		}

		for _, p := range sysPartitions(fsys, e.Name()) {
			if n := readSysFile(fsys, path.Join("sys/block", e.Name(), p, "dev")); n != "" {
				out[n] = p
			}
		}
	}

	return out
}

// readMountEntries returns the mounts of devices under /dev, in the order of
// /proc/self/mountinfo. kname takes the device path and number.
func readMountEntries(fsys fs.FS, kname func(dev, number string) string) ([]mountEntry, error) {
	b, err := fs.ReadFile(fsys, "proc/self/mountinfo")
	if err != nil {
		return nil, errors.Wrap(err, "unable to read the mounts")
	}

	var out []mountEntry

	for line := range strings.SplitSeq(string(b), "\n") {
		// id parent major:minor root mount-point options [optional...] - type source super-options
		f := strings.Fields(line)

		sep := slices.Index(f, "-")
		if sep < 5 || sep+2 >= len(f) {
			continue
		}

		if dev := unescapeMountField(f[sep+2]); strings.HasPrefix(dev, "/dev/") {
			out = append(out, mountEntry{dev: dev, kname: kname(dev, f[2]), dir: unescapeMountField(f[4])})
		}
	}

	return out, nil
}

// unescapeMountField undoes the octal escapes of spaces, tabs, newlines and
// backslashes in /proc/self/mountinfo.
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

// readSwaps returns the kernel names of the devices in /proc/swaps, which
// does not exist on a kernel without swap.
func readSwaps(fsys fs.FS, kname func(dev, number string) string) ([]string, error) {
	b, err := fs.ReadFile(fsys, "proc/swaps")
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}

	if err != nil {
		return nil, errors.Wrap(err, "unable to read the swap devices")
	}

	var out []string

	for line := range strings.SplitSeq(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) >= 1 && strings.HasPrefix(f[0], "/dev/") {
			out = append(out, kname(unescapeMountField(f[0]), ""))
		}
	}

	return out, nil
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

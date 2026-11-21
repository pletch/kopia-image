package disksource

import (
	"errors"
	"slices"
	"strings"

	"golang.org/x/sys/windows"

	"github.com/kopia/kopia/snapshot/image"
	"github.com/kopia/kopia/snapshot/image/blockdev"
)

const maxVolumeNameLength = windows.MAX_PATH + 1

// LocalDisks returns the disks that hold a fixed volume with a drive letter,
// ordered by disk number. It needs no administrator rights. Volumes that span
// several disks, such as on dynamic disks, volumes on no physical disk, such
// as Google Drive's virtual drive, and Storage Spaces pool members are left
// out: kopia image cannot image them.
func LocalDisks() ([]LocalDisk, error) {
	mask, err := windows.GetLogicalDrives()
	if err != nil {
		return nil, err //nolint:wrapcheck
	}

	systemDrive := ""
	if dir, err := windows.GetWindowsDirectory(); err == nil && len(dir) >= 2 {
		systemDrive = strings.ToUpper(dir[:2])
	}

	byNumber := map[uint32]*LocalDisk{}

	for i := range 26 {
		if mask&(1<<i) == 0 {
			continue
		}

		letter := string(rune('A'+i)) + ":"

		root, err := windows.UTF16PtrFromString(letter + `\`)
		if err != nil || windows.GetDriveType(root) != windows.DRIVE_FIXED {
			continue
		}

		extents, err := blockdev.VolumeDiskExtents(letter)
		if err != nil {
			continue
		}

		n, err := singleDisk(extents)
		if err != nil {
			continue
		}

		if _, ok := byNumber[n]; !ok && isUnsupportedDisk(n) {
			continue
		}

		d := byNumber[n]
		if d == nil {
			d = &LocalDisk{Number: n}
			if desc, err := blockdev.DescribeDisk(n); err == nil {
				d.Model, d.Bus, d.Removable = desc.Model, desc.Bus, desc.Removable
			}

			byNumber[n] = d
		}

		v := LocalVolume{Letter: letter}
		v.Label, v.FileSystem = volumeInformation(root)

		var total uint64
		if err := windows.GetDiskFreeSpaceEx(root, nil, &total, nil); err == nil {
			v.Size = int64(total)
		}

		d.Volumes = append(d.Volumes, v)
		d.System = d.System || letter == systemDrive
	}

	out := make([]LocalDisk, 0, len(byNumber))
	for _, d := range byNumber {
		out = append(out, *d)
	}

	slices.SortFunc(out, func(a, b LocalDisk) int { return int(a.Number) - int(b.Number) })

	return out, nil
}

// isUnsupportedDisk reports whether a disk's partition table marks it as a
// Storage Spaces pool member or a dynamic disk: imaging such a disk alone
// captures only part of its volumes. A disk whose table cannot be read for
// another reason is not ruled out here; image creation reports that.
func isUnsupportedDisk(diskNumber uint32) bool {
	_, err := blockdev.ReadLayout(diskNumber)

	return errors.Is(err, image.ErrUnsupportedLayout)
}

func volumeInformation(root *uint16) (label, fileSystem string) {
	labelBuf := make([]uint16, maxVolumeNameLength)
	fsBuf := make([]uint16, maxVolumeNameLength)

	if err := windows.GetVolumeInformation(root, &labelBuf[0], maxVolumeNameLength, nil, nil, nil, &fsBuf[0], maxVolumeNameLength); err != nil {
		return "", ""
	}

	return windows.UTF16ToString(labelBuf), windows.UTF16ToString(fsBuf)
}

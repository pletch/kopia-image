package imagerestore

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/pkg/errors"

	"github.com/kopia/kopia/internal/units"
)

// RecoveryMediaLabel is the volume label of kopia recovery media. A disk
// holding such a volume is never offered as a restore target.
const RecoveryMediaLabel = "KOPIARE"

// DiskVolume is a volume on a disk, shown so that a person can recognize the
// disk before it is erased.
type DiskVolume struct {
	FileSystem  string
	Label       string
	MountPoints []string
}

// DiskInfo describes a whole disk that an image could be restored to.
type DiskInfo struct {
	// Path is what "kopia image restore --target" takes.
	Path string

	Model     string
	Bus       string
	Removable bool

	// Serial is the serial number or other unique identifier the disk
	// reports, or "".
	Serial string

	// Size and SectorSize are in bytes.
	Size       int64
	SectorSize int64

	// DiskGUID is the GPT disk GUID, or "" for other disks.
	DiskGUID string

	Volumes []DiskVolume

	// InUse is set on Linux when the disk is mounted, used as swap, held by
	// another block device or part of a mounted btrfs file system, which
	// keeps it from being opened exclusively for a restore.
	InUse bool
}

// IsRecoveryMedia reports whether the disk holds kopia recovery media.
func (d *DiskInfo) IsRecoveryMedia() bool {
	for _, v := range d.Volumes {
		if strings.EqualFold(v.Label, RecoveryMediaLabel) {
			return true
		}
	}

	return false
}

// HasMountPoint reports whether one of the disk's volumes is mounted at, or
// holds, the given path, as given or with its symbolic links resolved. Paths
// are compared without regard to case, slash direction or trailing
// separators, so that C:\Backups is under the mount point C:\Backups\ that
// Windows reports, and E:/kopia under E:\.
func (d *DiskInfo) HasMountPoint(path string) bool {
	paths := []string{path}
	if r, err := filepath.EvalSymlinks(path); err == nil && r != path {
		paths = append(paths, r)
	}

	for _, v := range d.Volumes {
		for _, m := range v.MountPoints {
			for _, p := range paths {
				if m != "" && pathIsUnder(p, m) {
					return true
				}
			}
		}
	}

	return false
}

// pathIsUnder reports whether path is dir or is inside it, so that /mnt/repo2
// is not inside /mnt/repo.
func pathIsUnder(path, dir string) bool {
	path, dir = normalizePath(path), normalizePath(dir)

	return path == dir || strings.HasPrefix(path, strings.TrimSuffix(dir, "/")+"/")
}

// normalizePath lowercases a path, turns backslashes into slashes and
// removes trailing slashes, except from "/".
func normalizePath(p string) string {
	p = strings.ToLower(strings.ReplaceAll(p, `\`, "/"))
	if t := strings.TrimRight(p, "/"); t != "" {
		return t
	}

	return p
}

// Describe returns a one-line description of the disk.
func (d *DiskInfo) Describe() string {
	model := d.Model
	if model == "" {
		model = "unknown model"
	}

	s := fmt.Sprintf("%v  %v  %v", d.Path, units.BytesString(d.Size), model)
	if d.Bus != "" {
		s += "  (" + d.Bus + ")"
	}

	return s
}

// DescribeVolumes returns a line per volume, or a line saying the disk is empty.
func (d *DiskInfo) DescribeVolumes() []string {
	if len(d.Volumes) == 0 {
		return []string{"no volumes"}
	}

	out := make([]string, 0, len(d.Volumes))

	for _, v := range d.Volumes {
		s := v.FileSystem
		if v.Label != "" {
			s += " \"" + v.Label + "\""
		}

		if len(v.MountPoints) > 0 {
			s += " at " + strings.Join(v.MountPoints, ", ")
		}

		out = append(out, s)
	}

	return out
}

// TargetProblem says why a disk cannot receive an image of a disk with the
// given minimum size and sector size, or returns "" if it can. excludePaths
// are paths that must survive the restore, such as a repository on a local
// disk.
func (d *DiskInfo) TargetProblem(minSize, sectorSize int64, excludePaths ...string) string {
	switch {
	case d.IsRecoveryMedia():
		return "the recovery media"
	case d.SectorSize != sectorSize:
		return fmt.Sprintf("has %v-byte sectors, the image needs %v", d.SectorSize, sectorSize)
	case d.Size < minSize:
		return fmt.Sprintf("too small, the image needs %v", units.BytesString(minSize))
	}

	for _, p := range excludePaths {
		if p != "" && d.HasMountPoint(p) {
			return "holds " + p
		}
	}

	if d.InUse {
		return "in use: " + strings.Join(d.DescribeVolumes(), "; ")
	}

	return ""
}

// ErrDiskChanged is returned by CheckSameDisk when the disk at a path is not
// the disk that was listed there.
var ErrDiskChanged = errors.New("the disk changed since it was listed")

// CheckSameDisk lists the disks again and checks that the disk at listed.Path
// is the one that was listed: the same size, sector size, model, bus, serial
// number, disk GUID and volumes. A disk unplugged and another attached in the
// meantime can take over its path, such as \.\PhysicalDrive2 or /dev/sdb,
// so this is called right before a listed disk is erased.
func CheckSameDisk(listed *DiskInfo) error {
	disks, err := ListDisks()
	if err != nil {
		return errors.Wrap(err, "unable to list the disks again")
	}

	i := slices.IndexFunc(disks, func(d DiskInfo) bool { return strings.EqualFold(d.Path, listed.Path) })
	if i < 0 {
		return errors.Wrapf(ErrDiskChanged, "%v is no longer attached", listed.Path)
	}

	if diff := listed.differences(&disks[i]); diff != "" {
		return errors.Wrapf(ErrDiskChanged, "%v: %v", listed.Path, diff)
	}

	return nil
}

// differences describes how another listing of a disk differs from d, or
// returns "".
func (d *DiskInfo) differences(now *DiskInfo) string {
	switch {
	case d.Size != now.Size || d.SectorSize != now.SectorSize:
		return fmt.Sprintf("it now has %v bytes in %v-byte sectors, it had %v in %v-byte sectors", now.Size, now.SectorSize, d.Size, d.SectorSize)
	case d.Model != now.Model || d.Bus != now.Bus || d.Serial != now.Serial:
		return fmt.Sprintf("it is now %q (%v, serial %q), it was %q (%v, serial %q)", now.Model, now.Bus, now.Serial, d.Model, d.Bus, d.Serial)
	case !strings.EqualFold(d.DiskGUID, now.DiskGUID):
		return fmt.Sprintf("its disk GUID is now %q, it was %q", now.DiskGUID, d.DiskGUID)
	case !slices.EqualFunc(d.Volumes, now.Volumes, sameVolume):
		return fmt.Sprintf("it now holds %v, it held %v", strings.Join(now.DescribeVolumes(), "; "), strings.Join(d.DescribeVolumes(), "; "))
	}

	return ""
}

func sameVolume(a, b DiskVolume) bool {
	return a.FileSystem == b.FileSystem && a.Label == b.Label && slices.Equal(a.MountPoints, b.MountPoints)
}

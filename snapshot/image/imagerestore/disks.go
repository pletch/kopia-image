package imagerestore

import (
	"fmt"
	"strings"

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

	// Size and SectorSize are in bytes.
	Size       int64
	SectorSize int64

	Volumes []DiskVolume
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
// holds, the given path.
func (d *DiskInfo) HasMountPoint(path string) bool {
	for _, v := range d.Volumes {
		for _, m := range v.MountPoints {
			if m != "" && strings.HasPrefix(strings.ToLower(path), strings.ToLower(m)) {
				return true
			}
		}
	}

	return false
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

	return ""
}

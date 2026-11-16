// Package disksource turns a Windows disk into the volume sources of a kopia
// image: it decides how each partition is imaged, creates and removes the
// shadow copies, and guards a run against sleep and against shadow copies
// left behind by an earlier run that crashed.
//
// Planning is platform independent; everything else is Windows-only and
// returns ErrNotSupported elsewhere.
package disksource

import (
	"fmt"
	"io"
	"strings"

	"github.com/pkg/errors"

	"github.com/kopia/kopia/snapshot/image"
)

// ErrNotSupported is returned on platforms where disks cannot be imaged.
var ErrNotSupported = errors.New("kopia image create is supported only on Windows")

// VolumeInfo describes a mounted volume on the disk being planned.
type VolumeInfo struct {
	// GUIDPath is the volume GUID path with a trailing backslash.
	GUIDPath string

	// FileSystem is the file system name, e.g. "NTFS", "FAT32" or "ReFS".
	FileSystem string

	// MountPoints are the drive letters ("C:\") and folders the volume is
	// mounted at.
	MountPoints []string

	// Offset and Length locate the volume on the disk in bytes.
	Offset int64
	Length int64
}

// PartitionPlan says how one partition is imaged.
type PartitionPlan struct {
	Partition image.Partition
	Kind      image.VolumeKind
	Label     string

	// Volume is set for partitions imaged from a shadow copy.
	Volume *VolumeInfo

	// Warning, when set, is shown to the user.
	Warning string
}

// EntryName returns the name of the partition's image in the snapshot.
func (p *PartitionPlan) EntryName() string {
	return image.VolumeEntryName(p.Partition.Number, p.Label)
}

// Plan is how a disk is imaged.
type Plan struct {
	DiskNumber uint32
	Layout     *image.Layout

	// Partitions are the partitions to image, ordered by number.
	Partitions []PartitionPlan

	// Skipped are partitions that hold no data, such as the Microsoft
	// reserved partition, which restore recreates from the layout.
	Skipped []image.Partition
}

// BuildPlan decides how to image each partition of a disk given the volumes
// mounted on it. NTFS volumes in basic data partitions are imaged from shadow
// copies; partitions without such a volume, such as the EFI system and
// recovery partitions, are read raw twice. ReFS volumes are refused.
func BuildPlan(diskNumber uint32, layout *image.Layout, volumes []VolumeInfo) (*Plan, error) {
	if err := layout.CheckSupported(); err != nil {
		return nil, errors.Wrapf(err, "disk %v", diskNumber)
	}

	plan := &Plan{DiskNumber: diskNumber, Layout: layout}

	for _, p := range layout.Partitions {
		if p.TypeGUID == image.PartitionTypeMSR {
			plan.Skipped = append(plan.Skipped, p)
			continue
		}

		pp, err := planPartition(p, volumeAt(volumes, p))
		if err != nil {
			return nil, errors.Wrapf(err, "disk %v partition %v", diskNumber, p.Number)
		}

		plan.Partitions = append(plan.Partitions, pp)
	}

	if len(plan.Partitions) == 0 {
		return nil, errors.Errorf("disk %v has no partitions to image", diskNumber)
	}

	return plan, nil
}

func volumeAt(volumes []VolumeInfo, p image.Partition) *VolumeInfo {
	for i := range volumes {
		if volumes[i].Offset == p.Offset {
			return &volumes[i]
		}
	}

	return nil
}

func planPartition(p image.Partition, v *VolumeInfo) (PartitionPlan, error) {
	if v != nil && strings.EqualFold(v.FileSystem, "ReFS") {
		return PartitionPlan{}, errors.Wrap(image.ErrUnsupportedLayout, "ReFS volumes are not supported")
	}

	if v != nil && v.Length > p.Length {
		return PartitionPlan{}, errors.Errorf("volume %v is larger than its partition", v.GUIDPath)
	}

	switch {
	case p.TypeGUID == image.PartitionTypeBasicData && v != nil && strings.EqualFold(v.FileSystem, "NTFS"):
		return PartitionPlan{Partition: p, Kind: image.VolumeKindNTFS, Label: volumeLabel(v), Volume: v}, nil

	case p.TypeGUID == image.PartitionTypeBasicData && v != nil:
		return PartitionPlan{
			Partition: p, Kind: image.VolumeKindRaw, Label: volumeLabel(v),
			Warning: fmt.Sprintf("%v volume %v has no shadow copy support in kopia image and is read raw; it must not change during the run", v.FileSystem, describeVolume(v)),
		}, nil

	case p.TypeGUID == image.PartitionTypeEFISystem:
		return PartitionPlan{Partition: p, Kind: image.VolumeKindRaw, Label: "esp"}, nil

	case p.TypeGUID == image.PartitionTypeWinRecovery:
		return PartitionPlan{Partition: p, Kind: image.VolumeKindRaw, Label: "recovery"}, nil

	case p.TypeGUID == image.PartitionTypeBasicData:
		return PartitionPlan{
			Partition: p, Kind: image.VolumeKindRaw, Label: "data",
			Warning: "basic data partition without a mounted volume is read raw",
		}, nil

	default:
		return PartitionPlan{
			Partition: p, Kind: image.VolumeKindRaw, Label: "part",
			Warning: fmt.Sprintf("partition type %v is not recognized and is read raw", p.TypeGUID),
		}, nil
	}
}

// volumeLabel names an image after the volume's drive letter, or "data".
func volumeLabel(v *VolumeInfo) string {
	for _, m := range v.MountPoints {
		if len(m) >= 2 && m[1] == ':' && (len(m) == 2 || m[2:] == `\`) {
			return strings.ToUpper(m[:1])
		}
	}

	return "data"
}

func describeVolume(v *VolumeInfo) string {
	if len(v.MountPoints) > 0 {
		return v.MountPoints[0]
	}

	return v.GUIDPath
}

// Print writes a human-readable description of the plan.
func (p *Plan) Print(w io.Writer) {
	fmt.Fprintf(w, "Disk %v (GPT %v, %v bytes, %v-byte sectors):\n", p.DiskNumber, p.Layout.DiskGUID, p.Layout.DiskSize, p.Layout.SectorSize) //nolint:errcheck

	for i := range p.Partitions {
		pp := &p.Partitions[i]

		how := "raw, read twice"
		if pp.Kind == image.VolumeKindNTFS {
			how = "NTFS from a shadow copy of " + describeVolume(pp.Volume)
		}

		fmt.Fprintf(w, "  partition %v %-28q %12v bytes -> %v (%v)\n", pp.Partition.Number, pp.Partition.Name, pp.Partition.Length, pp.EntryName(), how) //nolint:errcheck

		if pp.Warning != "" {
			fmt.Fprintf(w, "    warning: %v\n", pp.Warning) //nolint:errcheck
		}
	}

	for _, s := range p.Skipped {
		fmt.Fprintf(w, "  partition %v %-28q %12v bytes -> not imaged, recreated on restore\n", s.Number, s.Name, s.Length) //nolint:errcheck
	}
}

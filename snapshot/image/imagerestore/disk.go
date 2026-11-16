package imagerestore

import (
	"context"
	"encoding/json"
	"io"
	"slices"
	"strconv"

	"github.com/pkg/errors"

	"github.com/kopia/kopia/fs"
	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/snapshot"
	"github.com/kopia/kopia/snapshot/image"
	"github.com/kopia/kopia/snapshot/image/blockdev"
	"github.com/kopia/kopia/snapshot/image/gpt"
	"github.com/kopia/kopia/snapshot/snapshotfs"
)

const maxMetadataFileSize = 64 << 20

// ErrDuplicateDiskGUID is returned when another attached disk has the imaged
// disk's GUID, which confuses Windows boot when both are attached.
var ErrDuplicateDiskGUID = errors.New("another attached disk has the same disk GUID")

// Snapshot is an image snapshot's layout and metadata.
type Snapshot struct {
	Manifest *snapshot.Manifest
	Layout   *image.Layout
	Metadata *image.Metadata
}

// LoadSnapshot reads and validates the layout.json and image.json of an image
// snapshot.
func LoadSnapshot(ctx context.Context, rep repo.Repository, man *snapshot.Manifest) (*Snapshot, error) {
	root, err := snapshotfs.SnapshotRoot(rep, man)
	if err != nil {
		return nil, errors.Wrap(err, "unable to open the snapshot")
	}

	dir, ok := root.(fs.Directory)
	if !ok {
		return nil, errors.New("snapshot root is not a directory")
	}

	s := &Snapshot{Manifest: man, Layout: &image.Layout{}, Metadata: &image.Metadata{}}

	if err := readJSONFile(ctx, dir, image.LayoutFileName, s.Layout); err != nil {
		return nil, err
	}

	if err := readJSONFile(ctx, dir, image.MetadataFileName, s.Metadata); err != nil {
		return nil, err
	}

	if err := s.Layout.Validate(); err != nil {
		return nil, errors.Wrap(err, "invalid layout.json")
	}

	if err := s.Metadata.Validate(s.Layout); err != nil {
		return nil, errors.Wrap(err, "invalid image.json")
	}

	return s, nil
}

func readJSONFile(ctx context.Context, dir fs.Directory, name string, v any) error {
	e, err := dir.Child(ctx, name)
	if errors.Is(err, fs.ErrEntryNotFound) {
		return errors.Errorf("%v not found; is this an image snapshot?", name)
	}

	if err != nil {
		return errors.Wrapf(err, "unable to read %v", name)
	}

	f, ok := e.(fs.File)
	if !ok {
		return errors.Errorf("%v is not a file", name)
	}

	r, err := f.Open(ctx)
	if err != nil {
		return errors.Wrapf(err, "unable to open %v", name)
	}
	defer r.Close() //nolint:errcheck

	b, err := io.ReadAll(io.LimitReader(r, maxMetadataFileSize))
	if err != nil {
		return errors.Wrapf(err, "unable to read %v", name)
	}

	return errors.Wrapf(json.Unmarshal(b, v), "invalid %v", name)
}

// SelectVolumes returns the volumes to restore: all of them when only is
// empty, otherwise those whose entry name ("p3-C.img"), name without
// extension ("p3-C") or partition number ("3") is listed.
func (s *Snapshot) SelectVolumes(only []string) ([]image.Volume, error) {
	if len(only) == 0 {
		return s.Metadata.Volumes, nil
	}

	var out []image.Volume

	for _, name := range only {
		i := slices.IndexFunc(s.Metadata.Volumes, func(v image.Volume) bool {
			return v.EntryName == name || v.EntryName == name+".img" || strconv.Itoa(v.PartitionNumber) == name
		})
		if i < 0 {
			return nil, errors.Errorf("the snapshot has no volume %q", name)
		}

		if !slices.ContainsFunc(out, func(v image.Volume) bool { return v.PartitionNumber == s.Metadata.Volumes[i].PartitionNumber }) {
			out = append(out, s.Metadata.Volumes[i])
		}
	}

	return out, nil
}

// CheckTarget checks that a target can hold the disk: the same logical sector
// size, room for every partition and the backup GPT, and no other attached
// disk with the same disk GUID unless allowDuplicateGUID.
func (s *Snapshot) CheckTarget(t Target, allowDuplicateGUID bool) error {
	if t.SectorSize() != s.Layout.SectorSize {
		return errors.Errorf("%v has %v-byte sectors; the image needs %v-byte sectors", t.Path(), t.SectorSize(), s.Layout.SectorSize)
	}

	if t.Size() < s.Layout.MinTargetSize() {
		return errors.Errorf("%v is %v bytes; the image needs at least %v bytes", t.Path(), t.Size(), s.Layout.MinTargetSize())
	}

	if !t.IsDevice() || allowDuplicateGUID {
		return nil
	}

	others, err := otherDiskGUIDs(t)
	if err != nil {
		return err
	}

	if path, ok := others[s.Layout.DiskGUID]; ok {
		return errors.Wrapf(ErrDuplicateDiskGUID, "%v has disk GUID %v; detach it or pass --force", path, s.Layout.DiskGUID)
	}

	return nil
}

// DiskOptions configure RestoreDisk.
type DiskOptions struct {
	// Volumes to restore; see SelectVolumes. When it is a subset of the
	// image's volumes, the partition table is not rewritten and the target's
	// table must already match for the selected partitions.
	Volumes []image.Volume

	Parallel int

	// Progress, when set, receives the volume entry name and bytes done.
	Progress func(entry string, done, total int64)
}

// VolumeResult is the outcome of restoring one volume.
type VolumeResult struct {
	Volume image.Volume
	Stats  Stats
}

// RestoreDisk restores an image to a target: it clears any partition table,
// writes every selected volume with parallel chunk fetches, zeroes partitions
// that have no image (such as the Microsoft reserved partition), and only then
// writes the partition table with the original GUIDs, syncs, verifies it and
// makes the operating system reread the partitions.
//
// Writing the partition table last keeps the operating system from finding
// and mounting partitions while their data is being written, and leaves a
// disk without a partition table if the restore is interrupted.
func RestoreDisk(ctx context.Context, rep repo.Repository, s *Snapshot, t Target, opt DiskOptions) ([]VolumeResult, error) {
	fullRestore := len(opt.Volumes) == len(s.Metadata.Volumes)

	if fullRestore {
		if err := gpt.Clear(t, t.SectorSize(), t.Size()); err != nil {
			return nil, errors.Wrap(err, "unable to clear the partition table")
		}
	} else if err := s.checkExistingTable(t, opt.Volumes); err != nil {
		return nil, err
	}

	results, err := restoreVolumes(ctx, rep, t, s.Layout, opt)
	if err != nil {
		return results, err
	}

	if fullRestore {
		return results, s.finishFullRestore(t, opt.Volumes)
	}

	return results, errors.Wrap(t.Sync(), "unable to sync")
}

func restoreVolumes(ctx context.Context, rep repo.Repository, t Target, layout *image.Layout, opt DiskOptions) ([]VolumeResult, error) {
	var results []VolumeResult

	for i := range opt.Volumes {
		v := opt.Volumes[i]
		p, _ := layout.Partition(v.PartitionNumber)

		// image.json was validated, so v.Size fits the partition; requiring
		// the object to be exactly that long keeps it inside the partition
		stats, err := WriteObject(ctx, rep, v.ObjectID, t, p.Offset, Options{
			ExpectedLength: v.Size,
			Parallel:       opt.Parallel,
			TargetIsZeroed: t.IsZeroed(),
			Zero:           t.Zero,
			NewBuffer:      blockdev.AlignedBuffer,
			Progress: func(done int64) {
				if opt.Progress != nil {
					opt.Progress(v.EntryName, done, v.Size)
				}
			},
		})
		if err != nil {
			return results, errors.Wrapf(err, "unable to restore %v", v.EntryName)
		}

		// a volume can be shorter than its partition; what the target held
		// there before must not survive the restore
		if tail := p.Length - v.Size; tail > 0 && !t.IsZeroed() {
			if err := t.Zero(p.Offset+v.Size, tail); err != nil {
				return results, errors.Wrapf(err, "unable to zero the end of partition %v", p.Number)
			}
		}

		results = append(results, VolumeResult{Volume: v, Stats: stats})
	}

	return results, nil
}

// finishFullRestore zeroes the partitions without an image, then writes,
// syncs and verifies the partition table and makes the operating system
// reread it.
func (s *Snapshot) finishFullRestore(t Target, volumes []image.Volume) error {
	if !t.IsZeroed() {
		if err := s.zeroPartitionsWithoutImage(t); err != nil {
			return err
		}
	}

	if err := gpt.Write(t, s.Layout, t.Size()); err != nil {
		return errors.Wrap(err, "unable to write the partition table")
	}

	if err := t.Sync(); err != nil {
		return errors.Wrap(err, "unable to sync")
	}

	if err := s.checkExistingTable(t, volumes); err != nil {
		return errors.Wrap(err, "partition table verification failed")
	}

	return errors.Wrap(t.RereadPartitions(), "unable to reread the partitions")
}

// checkExistingTable checks that the target's partition table matches the
// layout for the given volumes' partitions.
func (s *Snapshot) checkExistingTable(t Target, volumes []image.Volume) error {
	table, err := gpt.Read(t, t.SectorSize())
	if err != nil {
		return errors.Wrapf(err, "unable to read the partition table of %v", t.Path())
	}

	if table.DiskGUID != s.Layout.DiskGUID {
		return errors.Errorf("%v has disk GUID %v, the image has %v", t.Path(), table.DiskGUID, s.Layout.DiskGUID)
	}

	for i := range volumes {
		want, _ := s.Layout.Partition(volumes[i].PartitionNumber)

		i := slices.IndexFunc(table.Partitions, func(p image.Partition) bool { return p.Number == want.Number })
		if i < 0 || table.Partitions[i] != want {
			return errors.Errorf("partition %v of %v does not match the image", want.Number, t.Path())
		}
	}

	return nil
}

func (s *Snapshot) zeroPartitionsWithoutImage(t Target) error {
	for _, p := range s.Layout.Partitions {
		if slices.ContainsFunc(s.Metadata.Volumes, func(v image.Volume) bool { return v.PartitionNumber == p.Number }) {
			continue
		}

		if err := t.Zero(p.Offset, p.Length); err != nil {
			return errors.Wrapf(err, "unable to zero partition %v", p.Number)
		}
	}

	return nil
}

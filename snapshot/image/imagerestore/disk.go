package imagerestore

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"io"
	"math"
	"slices"
	"strconv"

	"github.com/pkg/errors"

	"github.com/kopia/kopia/fs"
	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/snapshot"
	"github.com/kopia/kopia/snapshot/image"
	"github.com/kopia/kopia/snapshot/image/blockdev"
	"github.com/kopia/kopia/snapshot/image/gpt"
	"github.com/kopia/kopia/snapshot/image/ntfsresize"
	"github.com/kopia/kopia/snapshot/snapshotfs"
)

const maxMetadataFileSize = 64 << 20

// ErrRereadPartitions is returned by RestoreDisk when the restore is complete
// and its partition table written and verified, but the operating system did
// not reread the partition table: it sees the new partitions once the disk is
// attached again or the computer restarts.
var ErrRereadPartitions = errors.New("the operating system did not reread the partition table")

// ErrDuplicateDiskGUID is returned when another attached disk has the imaged
// disk's GUID, which confuses Windows boot when both are attached.
var ErrDuplicateDiskGUID = errors.New("another attached disk has the same disk GUID")

// Snapshot is an image snapshot's layout and metadata.
type Snapshot struct {
	Manifest *snapshot.Manifest
	Layout   *image.Layout
	Metadata *image.Metadata

	// Resized are the partitions that Layout shrinks or moves to fit a
	// smaller target, see FitTo.
	Resized []Resize
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
	// image's volumes, or Partial is set, the partition table is not
	// rewritten and the target's table must already match for the selected
	// partitions.
	Volumes []image.Volume

	// Partial restores the volumes into the target's partition table, as for
	// a subset, also when they are all of the image's volumes.
	Partial bool

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
// that have no image (such as the Microsoft reserved partition), syncs, and
// only then writes the partition table with the original GUIDs, syncs again,
// verifies it and makes the operating system reread the partitions.
//
// Writing the partition table last keeps the operating system from finding
// and mounting partitions while their data is being written, and leaves a
// disk without a partition table if the restore is interrupted. The sync
// before it keeps a disk's write cache from storing the partition table
// before the data if the power fails.
func RestoreDisk(ctx context.Context, rep repo.Repository, s *Snapshot, t Target, opt DiskOptions) ([]VolumeResult, error) {
	if err := s.checkVolumes(opt.Volumes); err != nil {
		return nil, err
	}

	fullRestore := !opt.Partial && len(opt.Volumes) == len(s.Metadata.Volumes)

	if !fullRestore && len(s.Resized) > 0 {
		return nil, errors.New("only a whole disk can be restored to a smaller disk")
	}

	// free clusters need not be zeroed; without the bitmaps they are
	free, err := s.freeSpace(ctx, rep)
	if err != nil {
		free = nil
	}

	if !fullRestore {
		return s.restorePartial(ctx, rep, t, free, opt)
	}

	if err := gpt.Clear(t, t.SectorSize(), t.Size()); err != nil {
		return nil, errors.Wrap(err, "unable to clear the partition table")
	}

	// the cleared table must be on the disk before any volume data, or an
	// interruption could leave the old table pointing at partly new data
	if err := t.Sync(); err != nil {
		return nil, errors.Wrap(err, "unable to sync the cleared partition table")
	}

	results, err := restoreVolumes(ctx, rep, t, s.Layout, free, opt)
	if err != nil {
		return results, err
	}

	return results, s.finishFullRestore(t, opt.Volumes)
}

// restorePartial restores some volumes into the partitions of the target's
// existing partition table, which stays valid throughout. So that an
// interrupted restore does not leave a volume that mounts with partly old
// data, the sectors that identify each volume (see headRanges) are zeroed and
// synced first, and written last, after the rest of the volume is synced.
func (s *Snapshot) restorePartial(ctx context.Context, rep repo.Repository, t Target, free map[int][]image.Extent, opt DiskOptions) ([]VolumeResult, error) {
	if err := s.checkExistingTable(t, opt.Volumes); err != nil {
		return nil, err
	}

	d := &deferringTarget{Target: t}

	for i := range opt.Volumes {
		p, _ := s.Layout.Partition(opt.Volumes[i].PartitionNumber)

		for _, e := range headRanges(&opt.Volumes[i], p, s.Layout.SectorSize) {
			if err := t.Zero(e.Start, e.Length); err != nil {
				return nil, errors.Wrapf(err, "unable to zero the start of partition %v", p.Number)
			}

			d.ranges = append(d.ranges, deferredRange{off: e.Start, buf: blockdev.AlignedBuffer(int(e.Length))})
		}
	}

	if err := t.Sync(); err != nil {
		return nil, errors.Wrap(err, "unable to sync")
	}

	results, err := restoreVolumes(ctx, rep, d, s.Layout, free, opt)
	if err != nil {
		return results, err
	}

	if err := t.Sync(); err != nil {
		return results, errors.Wrap(err, "unable to sync the volumes")
	}

	for _, r := range d.ranges {
		if _, err := t.WriteAt(r.buf, r.off); err != nil {
			return results, errors.Wrapf(err, "unable to write %v bytes at offset %v", len(r.buf), r.off)
		}
	}

	return results, errors.Wrap(t.Sync(), "unable to sync")
}

// volumeHeadSize is how much of the start of a volume a partial restore
// writes last: the boot sector and its FAT backup, and the superblocks of
// other file systems.
const volumeHeadSize = 128 << 10

// headRanges returns the target ranges that make a partition recognizable as
// a volume: its first volumeHeadSize bytes and, for NTFS, the backup boot
// sector in the volume's last sector.
func headRanges(v *image.Volume, p image.Partition, sectorSize int64) []image.Extent {
	head := min(int64(volumeHeadSize), v.Size)
	out := []image.Extent{{Start: p.Offset, Length: head}}

	if v.Kind == image.VolumeKindNTFS && v.Size-sectorSize >= head {
		out = append(out, image.Extent{Start: p.Offset + v.Size - sectorSize, Length: sectorSize})
	}

	return out
}

// deferredRange is a range of a target whose writes are kept in buf.
type deferredRange struct {
	off int64
	buf []byte
}

// deferringTarget keeps the writes and zeroing of its ranges in memory and
// passes everything else to the target.
type deferringTarget struct {
	Target

	ranges []deferredRange
}

func (d *deferringTarget) WriteAt(p []byte, off int64) (int, error) {
	end := off + int64(len(p))

	err := d.split(off, end, func(r *deferredRange, from, to int64) {
		copy(r.buf[from-r.off:to-r.off], p[from-off:to-off])
	}, func(from, to int64) error {
		_, err := d.Target.WriteAt(p[from-off:to-off], from)
		return err //nolint:wrapcheck
	})
	if err != nil {
		return 0, err
	}

	return len(p), nil
}

func (d *deferringTarget) Zero(off, length int64) error {
	return d.split(off, off+length, func(r *deferredRange, from, to int64) {
		clear(r.buf[from-r.off : to-r.off])
	}, func(from, to int64) error {
		return d.Target.Zero(from, to-from)
	})
}

// split calls deferred for the parts of [off, end) inside a range and pass
// for the parts outside them, in order.
func (d *deferringTarget) split(off, end int64, deferred func(r *deferredRange, from, to int64), pass func(from, to int64) error) error {
	for off < end {
		if r := d.rangeAt(off); r != nil {
			next := min(end, r.off+int64(len(r.buf)))
			deferred(r, off, next)
			off = next

			continue
		}

		next := end

		for i := range d.ranges {
			if r := &d.ranges[i]; r.off > off && r.off < next {
				next = r.off
			}
		}

		if err := pass(off, next); err != nil {
			return err
		}

		off = next
	}

	return nil
}

func (d *deferringTarget) rangeAt(off int64) *deferredRange {
	for i := range d.ranges {
		if r := &d.ranges[i]; off >= r.off && off < r.off+int64(len(r.buf)) {
			return r
		}
	}

	return nil
}

// checkVolumes checks that volumes are volumes of the snapshot, each listed
// once, whose partitions are in its layout. RestoreDisk takes them for a full
// restore when they are as many as the snapshot's volumes, which they then
// are, each exactly once.
func (s *Snapshot) checkVolumes(volumes []image.Volume) error {
	if len(volumes) == 0 {
		return errors.New("no volumes to restore")
	}

	seen := map[int]bool{}

	for i := range volumes {
		v := &volumes[i]

		if seen[v.PartitionNumber] {
			return errors.Errorf("partition %v is listed more than once", v.PartitionNumber)
		}

		seen[v.PartitionNumber] = true

		j := slices.IndexFunc(s.Metadata.Volumes, func(m image.Volume) bool { return m.PartitionNumber == v.PartitionNumber })
		if j < 0 {
			return errors.Errorf("the snapshot has no volume for partition %v", v.PartitionNumber)
		}

		if m := &s.Metadata.Volumes[j]; m.EntryName != v.EntryName || m.ObjectID != v.ObjectID || m.Size != v.Size || m.Kind != v.Kind || m.ClusterSize != v.ClusterSize {
			return errors.Errorf("%v (partition %v) is not the snapshot's volume %v", v.EntryName, v.PartitionNumber, m.EntryName)
		}

		if _, ok := s.Layout.Partition(v.PartitionNumber); !ok {
			return errors.Errorf("partition %v of %v is not in the layout", v.PartitionNumber, v.EntryName)
		}
	}

	return nil
}

func restoreVolumes(ctx context.Context, rep repo.Repository, t Target, layout *image.Layout, free map[int][]image.Extent, opt DiskOptions) ([]VolumeResult, error) {
	var results []VolumeResult

	for i := range opt.Volumes {
		v := opt.Volumes[i]

		p, ok := layout.Partition(v.PartitionNumber)
		if !ok {
			return results, errors.Errorf("partition %v of %v is not in the layout", v.PartitionNumber, v.EntryName)
		}

		// an NTFS volume restored to a shorter partition, see FitTo
		shrink := v.Size > p.Length
		if shrink && v.Kind != image.VolumeKindNTFS {
			return results, errors.Errorf("%v is %v bytes, longer than partition %v, and is not NTFS", v.EntryName, v.Size, p.Number)
		}

		// image.json was validated, so v.Size fits the partition as imaged;
		// requiring the object to be exactly that long, and writing no more
		// than the partition on the target holds, keeps it inside the
		// partition
		stats, err := WriteObject(ctx, rep, v.ObjectID, t, p.Offset, Options{
			ExpectedLength: v.Size,
			Limit:          min(v.Size, p.Length),
			DataEnd:        ntfsClustersEnd(&v, layout.SectorSize),
			Unused:         free[v.PartitionNumber],
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

		if shrink {
			if err := ntfsresize.Shrink(&section{t: t, offset: p.Offset, length: p.Length}, p.Length); err != nil {
				return results, errors.Wrapf(err, "unable to shrink %v to %v bytes", v.EntryName, p.Length)
			}
		}

		// a partition that FitTo moved starts at another sector than the one
		// its boot sector records
		if v.Kind == image.VolumeKindNTFS && p.Offset/layout.SectorSize <= math.MaxUint32 {
			if err := ntfsresize.SetHiddenSectors(&section{t: t, offset: p.Offset, length: p.Length}, uint32(p.Offset/layout.SectorSize)); err != nil { //nolint:gosec
				return results, errors.Wrapf(err, "unable to update the boot sector of %v", v.EntryName)
			}
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

// ntfsClustersEnd returns where the clusters of an imaged NTFS volume end:
// the image is the whole partition, whose last sector holds the backup boot
// sector, and whose sectors past the last whole cluster hold nothing.
func ntfsClustersEnd(v *image.Volume, sectorSize int64) int64 {
	if v.Kind != image.VolumeKindNTFS || v.ClusterSize <= 0 {
		return v.Size
	}

	return (v.Size - sectorSize) / v.ClusterSize * v.ClusterSize
}

// section is a partition of a target.
type section struct {
	t      Target
	offset int64
	length int64
}

func (s *section) ReadAt(b []byte, off int64) (int, error) {
	if off < 0 || off+int64(len(b)) > s.length {
		return 0, errors.Errorf("read of %v bytes at %v outside the partition of %v bytes", len(b), off, s.length)
	}

	return s.t.ReadAt(b, s.offset+off) //nolint:wrapcheck
}

func (s *section) WriteAt(b []byte, off int64) (int, error) {
	if off < 0 || off+int64(len(b)) > s.length {
		return 0, errors.Errorf("write of %v bytes at %v outside the partition of %v bytes", len(b), off, s.length)
	}

	return s.t.WriteAt(b, s.offset+off) //nolint:wrapcheck
}

// finishFullRestore zeroes the partitions without an image and syncs, then
// writes, syncs and verifies the partition table and makes the operating
// system reread it.
func (s *Snapshot) finishFullRestore(t Target, volumes []image.Volume) error {
	if !t.IsZeroed() {
		if err := s.zeroPartitionsWithoutImage(t); err != nil {
			return err
		}
	}

	// the volumes must be on the disk before a partition table points at them
	if err := t.Sync(); err != nil {
		return errors.Wrap(err, "unable to sync the volumes")
	}

	if err := gpt.Write(t, s.Layout, t.Size()); err != nil {
		return errors.Wrap(err, "unable to write the partition table")
	}

	if err := t.Sync(); err != nil {
		return errors.Wrap(err, "unable to sync")
	}

	// read the table back from the disk, not from the operating system's cache
	if c, ok := t.(interface{ dropCache() error }); ok {
		if err := c.dropCache(); err != nil {
			return err
		}
	}

	if err := s.checkExistingTable(t, volumes); err != nil {
		return errors.Wrap(err, "partition table verification failed")
	}

	if err := t.RereadPartitions(); err != nil {
		return errors.Wrap(stderrors.Join(ErrRereadPartitions, err), "unable to reread the partitions")
	}

	return nil
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

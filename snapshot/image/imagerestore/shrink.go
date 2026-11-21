package imagerestore

import (
	"cmp"
	"context"
	"io"
	"math/bits"
	"slices"
	"strings"

	"github.com/pkg/errors"

	"github.com/kopia/kopia/fs"
	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/snapshot/image"
	"github.com/kopia/kopia/snapshot/image/ntfsresize"
	"github.com/kopia/kopia/snapshot/snapshotfs"
)

const (
	// shrinkAlignment is what partitions are shrunk and moved by, so that
	// partitions that were aligned stay aligned.
	shrinkAlignment = 1 << 20

	// maxShrinkFreeSpace is the most free space a shrunk NTFS volume keeps
	// at least, so that Windows has room to start and work; small volumes
	// keep a tenth of their size instead.
	maxShrinkFreeSpace = 1 << 30

	maxBitmapSize = 1 << 30
)

// ErrTargetTooSmall is returned when a target cannot hold an image even with
// its NTFS volumes shrunk.
var ErrTargetTooSmall = errors.New("the target is too small for the image")

// ShrinkLimits are, by partition number, the shortest partitions that the
// NTFS volumes of an image can be restored to: they hold every allocated
// cluster of the volume and some free space.
type ShrinkLimits map[int]int64

// LoadShrinkLimits reads the allocation bitmaps that an image stores next to
// its NTFS volumes and returns how far each volume can be shrunk. Volumes
// without a bitmap cannot be shrunk.
func (s *Snapshot) LoadShrinkLimits(ctx context.Context, rep repo.Repository) (ShrinkLimits, error) {
	bitmaps, err := s.loadBitmaps(ctx, rep)
	if err != nil {
		return nil, err
	}

	limits := ShrinkLimits{}

	for i := range s.Metadata.Volumes {
		v := &s.Metadata.Volumes[i]

		bitmap, ok := bitmaps[v.PartitionNumber]
		if !ok {
			continue
		}

		// the bitmap's bits past the last cluster are set, as in $Bitmap
		p, _ := s.Layout.Partition(v.PartitionNumber)
		clusters := ntfsClustersEnd(v, s.Layout.SectorSize) / v.ClusterSize
		limits[v.PartitionNumber] = shrinkLimit(bitmap, clusters, v.ClusterSize, s.Layout.SectorSize, p.Length)
	}

	return limits, nil
}

// loadBitmaps reads, by partition number, the allocation bitmaps that an
// image stores next to its NTFS volumes.
func (s *Snapshot) loadBitmaps(ctx context.Context, rep repo.Repository) (map[int][]byte, error) {
	root, err := snapshotfs.SnapshotRoot(rep, s.Manifest)
	if err != nil {
		return nil, errors.Wrap(err, "unable to open the snapshot")
	}

	dir, ok := root.(fs.Directory)
	if !ok {
		return nil, errors.New("snapshot root is not a directory")
	}

	out := map[int][]byte{}

	for i := range s.Metadata.Volumes {
		v := &s.Metadata.Volumes[i]
		if v.Kind != image.VolumeKindNTFS || v.ClusterSize <= 0 {
			continue
		}

		bitmap, err := readBitmapEntry(ctx, dir, strings.TrimSuffix(v.EntryName, ".img")+".bitmap")
		if err != nil {
			return nil, err
		}

		if bitmap == nil {
			continue
		}

		// the bitmap is read with image.json's cluster size, which must be
		// the volume's own, or used clusters would be taken for free ones
		if err := checkNTFSGeometry(ctx, rep, v, s.Layout.SectorSize); err != nil {
			return nil, err
		}

		out[v.PartitionNumber] = bitmap
	}

	return out, nil
}

// freeSpace returns, by partition number, the free clusters of the image's
// NTFS volumes, whose content NTFS ignores. Volumes without a bitmap are
// left out.
func (s *Snapshot) freeSpace(ctx context.Context, rep repo.Repository) (map[int][]image.Extent, error) {
	bitmaps, err := s.loadBitmaps(ctx, rep)
	if err != nil {
		return nil, err
	}

	out := map[int][]image.Extent{}

	for i := range s.Metadata.Volumes {
		v := &s.Metadata.Volumes[i]

		bitmap, ok := bitmaps[v.PartitionNumber]
		if !ok {
			continue
		}

		free, err := image.FreeExtents(bitmap, ntfsClustersEnd(v, s.Layout.SectorSize)/v.ClusterSize, v.ClusterSize)
		if err != nil {
			return nil, errors.Wrapf(err, "invalid bitmap of %v", v.EntryName)
		}

		out[v.PartitionNumber] = free
	}

	return out, nil
}

// shrinkLimit returns the shortest partition, a whole number of sectors, that
// holds an NTFS volume's allocated clusters and keeps some free space.
func shrinkLimit(bitmap []byte, totalClusters, clusterSize, sectorSize, partitionLength int64) int64 {
	var used int64

	whole := min(int64(len(bitmap)), totalClusters/8) //nolint:mnd

	for i := range whole {
		used += int64(bits.OnesCount8(bitmap[i]))
	}

	// the bits of the last clusters, without those past the end
	if rest := totalClusters % 8; rest > 0 && whole < int64(len(bitmap)) { //nolint:mnd
		used += int64(bits.OnesCount8(bitmap[whole] & (1<<rest - 1)))
	}

	free := min(maxShrinkFreeSpace, partitionLength/10) //nolint:mnd
	limit := max(ntfsresize.MinPartitionLength(bitmap, totalClusters, clusterSize, sectorSize), used*clusterSize+free+sectorSize)

	return min(partitionLength, (limit+sectorSize-1)/sectorSize*sectorSize)
}

// checkNTFSGeometry checks that the boot sector of an imaged NTFS volume has
// the cluster size and cluster count that image.json gives it.
func checkNTFSGeometry(ctx context.Context, rep repo.Repository, v *image.Volume, sectorSize int64) error {
	r, err := rep.OpenObject(ctx, v.ObjectID)
	if err != nil {
		return errors.Wrapf(err, "unable to open %v", v.EntryName)
	}
	defer r.Close() //nolint:errcheck

	sector := make([]byte, max(sectorSize, bootSectorReadSize))
	if _, err := io.ReadFull(r, sector); err != nil {
		return errors.Wrapf(err, "unable to read the boot sector of %v", v.EntryName)
	}

	b, err := ntfsresize.ParseBoot(sector)
	if err != nil {
		return errors.Wrapf(err, "invalid boot sector in %v", v.EntryName)
	}

	if clusters := ntfsClustersEnd(v, sectorSize) / v.ClusterSize; b.ClusterSize != v.ClusterSize || b.TotalClusters() != clusters {
		return errors.Errorf("%v has %v clusters of %v bytes, image.json says %v of %v bytes", v.EntryName, b.TotalClusters(), b.ClusterSize, clusters, v.ClusterSize)
	}

	return nil
}

// bootSectorReadSize is the least read for an NTFS boot sector.
const bootSectorReadSize = 512

func readBitmapEntry(ctx context.Context, dir fs.Directory, name string) ([]byte, error) {
	e, err := dir.Child(ctx, name)
	if errors.Is(err, fs.ErrEntryNotFound) {
		return nil, nil
	}

	if err != nil {
		return nil, errors.Wrapf(err, "unable to read %v", name)
	}

	f, ok := e.(fs.File)
	if !ok {
		return nil, errors.Errorf("%v is not a file", name)
	}

	r, err := f.Open(ctx)
	if err != nil {
		return nil, errors.Wrapf(err, "unable to open %v", name)
	}
	defer r.Close() //nolint:errcheck

	b, err := io.ReadAll(io.LimitReader(r, maxBitmapSize))

	return b, errors.Wrapf(err, "unable to read %v", name)
}

// Resize is how restoring to a smaller target changes a partition.
type Resize struct {
	Partition image.Partition // as imaged
	Offset    int64           // on the target
	Length    int64
}

// Shrunk reports whether the partition is shorter on the target.
func (r Resize) Shrunk() bool { return r.Length < r.Partition.Length }

// Moved reports whether the partition starts elsewhere on the target.
func (r Resize) Moved() bool { return r.Offset != r.Partition.Offset }

// MinTargetSize returns the smallest target that can hold the image with its
// NTFS volumes shrunk as far as limits allow.
func (s *Snapshot) MinTargetSize(limits ShrinkLimits) int64 {
	var slack int64

	for _, p := range s.Layout.Partitions {
		slack += shrinkSlack(p, limits)
	}

	return s.Layout.MinTargetSize() - slack
}

// shrinkSlack returns how much a partition can be shrunk, in whole
// alignment units.
func shrinkSlack(p image.Partition, limits ShrinkLimits) int64 {
	limit, ok := limits[p.Number]
	if !ok || limit >= p.Length {
		return 0
	}

	return (p.Length - limit) / shrinkAlignment * shrinkAlignment
}

// FitTo returns the snapshot to restore to a target of targetSize bytes: the
// snapshot itself when the target holds the disk as imaged, otherwise a copy
// whose layout shrinks NTFS partitions, the largest first and each no more
// than needed, and moves the partitions after them down. Partitions keep
// their order, numbers and GUIDs. It also returns the partitions that change.
func (s *Snapshot) FitTo(targetSize int64, limits ShrinkLimits) (*Snapshot, []Resize, error) {
	need := s.Layout.MinTargetSize() - targetSize
	if need <= 0 {
		return s, nil, nil
	}

	if minSize := s.MinTargetSize(limits); targetSize < minSize {
		return nil, nil, errors.Wrapf(ErrTargetTooSmall, "the image needs at least %v bytes", minSize)
	}

	cut := map[int]int64{}

	byLength := slices.Clone(s.Layout.Partitions)
	slices.SortStableFunc(byLength, func(a, b image.Partition) int { return cmp.Compare(b.Length, a.Length) })

	for _, p := range byLength {
		if need <= 0 {
			break
		}

		c := min(shrinkSlack(p, limits), (need+shrinkAlignment-1)/shrinkAlignment*shrinkAlignment)
		if c > 0 {
			cut[p.Number] = c
			need -= c
		}
	}

	layout := *s.Layout
	layout.DiskSize = targetSize
	layout.Partitions = slices.Clone(s.Layout.Partitions)

	byOffset := slices.Clone(s.Layout.Partitions)
	slices.SortFunc(byOffset, func(a, b image.Partition) int { return cmp.Compare(a.Offset, b.Offset) })

	var (
		shift   int64
		changes []Resize
	)

	for _, p := range byOffset {
		r := Resize{Partition: p, Offset: p.Offset - shift, Length: p.Length - cut[p.Number]}
		shift += cut[p.Number]

		i := slices.IndexFunc(layout.Partitions, func(q image.Partition) bool { return q.Number == p.Number })
		layout.Partitions[i].Offset, layout.Partitions[i].Length = r.Offset, r.Length

		if r.Shrunk() || r.Moved() {
			changes = append(changes, r)
		}
	}

	if err := layout.Validate(); err != nil {
		return nil, nil, errors.Wrap(err, "invalid layout for the smaller target")
	}

	if layout.MinTargetSize() > targetSize {
		return nil, nil, errors.Errorf("the shrunk layout needs %v bytes, the target has %v", layout.MinTargetSize(), targetSize)
	}

	out := *s
	out.Layout = &layout
	out.Resized = changes

	return &out, changes, nil
}

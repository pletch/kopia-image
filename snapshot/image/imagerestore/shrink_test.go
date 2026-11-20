package imagerestore

import (
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/internal/repotesting"
	"github.com/kopia/kopia/snapshot"
	"github.com/kopia/kopia/snapshot/image"
	"github.com/kopia/kopia/snapshot/image/gpt"
	"github.com/kopia/kopia/snapshot/image/ntfsresize"
)

const mib = 1 << 20

// shrinkLayout is a Windows disk of 64 MiB: ESP, MSR, a 32 MiB C: and a
// recovery partition after it.
func shrinkLayout() *image.Layout {
	return &image.Layout{
		DiskGUID:   "0f6ab3f2-1c3d-4e5f-8a9b-0c1d2e3f4a5b",
		SectorSize: 512,
		DiskSize:   64 * mib,
		Partitions: []image.Partition{
			{Number: 1, TypeGUID: image.PartitionTypeEFISystem, UniqueGUID: "11111111-2222-4333-8444-555555555551", Offset: 1 * mib, Length: 8 * mib, Name: "EFI system partition"},
			{Number: 2, TypeGUID: image.PartitionTypeMSR, UniqueGUID: "11111111-2222-4333-8444-555555555552", Offset: 9 * mib, Length: 16 * mib, Name: "Microsoft reserved partition"},
			{Number: 3, TypeGUID: image.PartitionTypeBasicData, UniqueGUID: "11111111-2222-4333-8444-555555555553", Offset: 25 * mib, Length: 32 * mib, Name: "Basic data partition"},
			{Number: 4, TypeGUID: image.PartitionTypeWinRecovery, UniqueGUID: "11111111-2222-4333-8444-555555555554", Offset: 57 * mib, Length: 4 * mib, Attributes: 1 << 63},
		},
	}
}

func shrinkSnapshot() *Snapshot {
	return &Snapshot{Layout: shrinkLayout(), Metadata: &image.Metadata{}}
}

func TestFitTo(t *testing.T) {
	s := shrinkSnapshot()
	full := s.Layout.MinTargetSize() // 61 MiB and the backup GPT

	t.Run("a target that holds the disk", func(t *testing.T) {
		got, changes, err := s.FitTo(full, ShrinkLimits{3: 20 * mib})
		require.NoError(t, err)
		require.Same(t, s, got)
		require.Empty(t, changes)
	})

	t.Run("shrinks C: by whole MiB and moves the recovery partition", func(t *testing.T) {
		got, changes, err := s.FitTo(58*mib, ShrinkLimits{3: 20 * mib})
		require.NoError(t, err)

		c, _ := got.Layout.Partition(3)
		require.Equal(t, 25*mib, int(c.Offset))
		require.Equal(t, 28*mib, int(c.Length), "4 MiB less: the 3 MiB and backup GPT needed, rounded up")

		rec, _ := got.Layout.Partition(4)
		require.Equal(t, 53*mib, int(rec.Offset))
		require.Equal(t, 4*mib, int(rec.Length))
		require.Equal(t, "11111111-2222-4333-8444-555555555554", rec.UniqueGUID)
		require.Equal(t, int64(58*mib), got.Layout.DiskSize)
		require.LessOrEqual(t, got.Layout.MinTargetSize(), int64(58*mib))

		require.Equal(t, []Resize{
			{Partition: s.Layout.Partitions[2], Offset: 25 * mib, Length: 28 * mib},
			{Partition: s.Layout.Partitions[3], Offset: 53 * mib, Length: 4 * mib},
		}, changes)
		require.True(t, changes[0].Shrunk())
		require.False(t, changes[0].Moved())
		require.True(t, changes[1].Moved())
		require.Equal(t, changes, got.Resized)

		// the snapshot itself is unchanged
		require.Equal(t, shrinkLayout(), s.Layout)
	})

	t.Run("too small", func(t *testing.T) {
		require.Equal(t, full-12*mib, s.MinTargetSize(ShrinkLimits{3: 20 * mib}))

		_, _, err := s.FitTo(full-12*mib-1, ShrinkLimits{3: 20 * mib})
		require.ErrorIs(t, err, ErrTargetTooSmall)

		_, _, err = s.FitTo(full-1, nil)
		require.ErrorIs(t, err, ErrTargetTooSmall, "nothing can shrink")
	})

	t.Run("the largest partition first", func(t *testing.T) {
		l := shrinkLayout()
		l.Partitions[1].TypeGUID = image.PartitionTypeBasicData // a 16 MiB D:

		got, _, err := (&Snapshot{Layout: l, Metadata: &image.Metadata{}}).FitTo(50*mib, ShrinkLimits{2: 10 * mib, 3: 24 * mib})
		require.NoError(t, err)

		d, _ := got.Layout.Partition(2)
		c, _ := got.Layout.Partition(3)
		require.Equal(t, 24*mib, int(c.Length), "C: as far as it goes")
		require.Equal(t, 12*mib, int(d.Length), "D: the rest, 3 MiB and the backup GPT rounded up")
		require.Equal(t, 21*mib, int(c.Offset))
	})
}

func TestShrinkLimit(t *testing.T) {
	// 100 clusters in use, the last at 199, of 1000
	bitmap := make([]byte, 125)
	for c := 100; c < 200; c++ {
		bitmap[c/8] |= 1 << (c % 8)
	}

	// up to the last cluster in use, as the free space is enough
	require.Equal(t, int64(200*4096+512), shrinkLimit(bitmap, 1000, 4096, 512, 1000*4096))

	// a large volume keeps 1 GiB free
	require.Equal(t, int64(100*4096+1<<30+512), shrinkLimit(bitmap, 1000, 4096, 512, 100<<30))

	// never more than the partition
	require.Equal(t, int64(150*4096), shrinkLimit(bitmap, 1000, 4096, 512, 150*4096))
}

func TestWriteObject_limit(t *testing.T) {
	ctx, env := repotesting.NewEnvironment(t, repotesting.FormatNotImportant)

	// 3 chunks of data, then 3 of zeros
	data := append(randomBytes(5, 3*chunk), make([]byte, 3*chunk)...)
	oid := writeTestObject(ctx, t, env.RepositoryWriter, data)

	for _, limit := range []int64{3 * chunk, 4*chunk + 512, 3*chunk - 0} {
		disk := &memDisk{data: bytes.Repeat([]byte{0xee}, 6*chunk)}

		var progress int64

		stats, err := WriteObject(ctx, env.Repository, oid, disk, 0, Options{
			Limit: limit, Parallel: 4, Zero: disk.zero,
			Progress: func(done int64) { progress = max(progress, done) },
		})
		require.NoError(t, err)
		require.Equal(t, data[:limit], disk.data[:limit])
		require.Equal(t, bytes.Repeat([]byte{0xee}, 6*chunk-int(limit)), disk.data[limit:], "nothing past the limit is written")
		require.Equal(t, limit-3*chunk, stats.ZeroBytes)
		require.Equal(t, int64(6*chunk), progress)
	}

	// data past the limit fails the restore, unless it is past DataEnd
	disk := &memDisk{data: make([]byte, 6*chunk)}

	_, err := WriteObject(ctx, env.Repository, oid, disk, 0, Options{Limit: chunk, DataEnd: chunk + 100, Zero: disk.zero})
	require.ErrorContains(t, err, "holds data past")

	_, err = WriteObject(ctx, env.Repository, oid, disk, 0, Options{Limit: chunk, DataEnd: chunk, Zero: disk.zero})
	require.NoError(t, err)
	require.Equal(t, data[:chunk], disk.data[:chunk])

	_, err = WriteObject(ctx, env.Repository, oid, disk, 0, Options{Limit: 2 * chunk, Zero: disk.zero})
	require.ErrorContains(t, err, "holds data past")

	_, err = WriteObject(ctx, env.Repository, oid, disk, 0, Options{Limit: 3*chunk - 512, Zero: disk.zero})
	require.ErrorContains(t, err, "holds data past")
}

func loadNTFSFixture(t *testing.T) []byte {
	t.Helper()

	f, err := os.Open("../ntfsresize/testdata/ntfs32m.img.gz")
	require.NoError(t, err)

	defer f.Close()

	r, err := gzip.NewReader(f)
	require.NoError(t, err)

	b, err := io.ReadAll(r)
	require.NoError(t, err)

	return b
}

// TestRestoreDisk_smallerTarget restores a disk whose C: is an NTFS volume
// that Windows formatted to a target too small for it, shrinking C: and
// moving the recovery partition.
func TestRestoreDisk_smallerTarget(t *testing.T) {
	ctx, env := repotesting.NewEnvironment(t, repotesting.FormatNotImportant)

	// kopia images an NTFS volume as its whole partition, with the backup
	// boot sector in its last sector
	ntfs := loadNTFSFixture(t)
	esp := randomBytes(7, 8*mib)
	recovery := randomBytes(8, 4*mib)
	layout := shrinkLayout()

	// clusters 0-5570 in use, as in the fixture, and the bit past the last
	// of its 8191 clusters set, as Windows returns it
	bitmap := make([]byte, 1024)
	for c := range 5571 {
		bitmap[c/8] |= 1 << (c % 8)
	}

	bitmap[1023] |= 0x80

	path, err := image.DiskSourcePath(layout.DiskGUID)
	require.NoError(t, err)

	man, _, err := image.Create(ctx, env.RepositoryWriter, &image.CreateOptions{
		Source: snapshot.SourceInfo{Host: "h", UserName: "u", Path: path},
		Layout: layout,
		Volumes: []image.VolumeSource{
			{PartitionNumber: 1, Label: "esp", Kind: image.VolumeKindRaw, Size: int64(len(esp)), Open: bytesSource(esp)},
			{PartitionNumber: 3, Label: "C", Kind: image.VolumeKindNTFS, Size: int64(len(ntfs)), Open: bytesSource(ntfs), ClusterSize: 4096, Bitmap: bitmap},
			{PartitionNumber: 4, Label: "recovery", Kind: image.VolumeKindRaw, Size: int64(len(recovery)), Open: bytesSource(recovery)},
		},
		Compressor: "zstd-fastest",
	})
	require.NoError(t, err)

	s, err := LoadSnapshot(ctx, env.Repository, man)
	require.NoError(t, err)

	limits, err := s.LoadShrinkLimits(ctx, env.Repository)
	require.NoError(t, err)
	// the clusters in use, a tenth of the volume free and the backup boot
	// sector, in whole sectors
	require.Equal(t, ShrinkLimits{3: (5571*4096 + 32*mib/10 + 512 + 511) / 512 * 512}, limits)

	// a disk of 56 MiB: C: loses 6 MiB
	targetPath := filepath.Join(t.TempDir(), "disk.img")

	target, err := OpenTarget(targetPath, 56*mib, 512)
	require.NoError(t, err)

	require.ErrorContains(t, s.CheckTarget(target, false), "needs at least")

	fit, changes, err := s.FitTo(target.Size(), limits)
	require.NoError(t, err)
	require.Len(t, changes, 2)
	require.NoError(t, fit.CheckTarget(target, false))

	_, err = RestoreDisk(ctx, env.Repository, fit, target, DiskOptions{Volumes: fit.Metadata.Volumes[:1]})
	require.ErrorContains(t, err, "only a whole disk")

	_, err = RestoreDisk(ctx, env.Repository, fit, target, DiskOptions{Volumes: fit.Metadata.Volumes, Parallel: 4})
	require.NoError(t, err)
	require.NoError(t, target.Close())

	f, err := os.Open(targetPath)
	require.NoError(t, err)

	defer f.Close()

	table, err := gpt.Read(f, 512)
	require.NoError(t, err)
	require.Equal(t, fit.Layout.Partitions, table.Partitions)

	c, _ := fit.Layout.Partition(3)
	require.Equal(t, int64(26*mib), c.Length)

	rec, _ := fit.Layout.Partition(4)
	require.Equal(t, recovery, readRange(t, targetPath, rec.Offset, rec.Length), "the recovery partition moved whole")
	require.Equal(t, esp, readRange(t, targetPath, 1*mib, 8*mib))

	info, err := ntfsresize.Inspect(io.NewSectionReader(f, c.Offset, c.Length))
	require.NoError(t, err)
	require.Equal(t, c.Length/512-1, info.Boot.TotalSectors)
	require.True(t, info.BackupBootMatches)
	require.True(t, info.Dirty)
	require.Equal(t, int64(5570), info.LastUsedCluster)

	// the files' clusters are as imaged
	restored := readRange(t, targetPath, c.Offset, c.Length)
	for cl := int64(3); cl < 5571; cl++ {
		if cl >= 2728 && cl <= 2732 { // $Bitmap and the first MFT records
			continue
		}

		require.True(t, bytes.Equal(ntfs[cl*4096:(cl+1)*4096], restored[cl*4096:(cl+1)*4096]), "cluster %v", cl)
	}
}

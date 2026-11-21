package ntfsresize

import (
	"encoding/binary"

	"github.com/pkg/errors"
)

// ErrInUse is returned when clusters past the new end of a volume are in
// use: shrinking it would need moving them, which this package does not do.
var ErrInUse = errors.New("clusters past the new end of the volume are in use")

// Shrink shrinks the NTFS volume on dev, which starts at offset 0 of dev, to
// fill a partition of partitionLength bytes, as Windows shrinks a volume
// whose clusters past the new end are all free: it sets the size in the boot
// sector and its backup, which goes to the last sector of the partition,
// cuts $Bitmap and $BadClus to the new number of clusters, and marks the
// volume for checking, so that Windows checks it when it first mounts it.
//
// It fails with ErrInUse, before writing anything, when a cluster past the
// new end is in use.
func Shrink(device Device, partitionLength int64) error {
	dev := &aligned{dev: device}

	v, err := open(dev)
	if err != nil {
		return err
	}

	b := v.boot

	if partitionLength%b.BytesPerSector != 0 {
		return errors.Errorf("partition length %v is not a multiple of the sector size %v", partitionLength, b.BytesPerSector)
	}

	totalSectors := partitionLength/b.BytesPerSector - 1
	clusterCount := totalSectors * b.BytesPerSector / b.ClusterSize

	if totalSectors > b.TotalSectors {
		return errors.Errorf("the volume has %v sectors, fewer than the %v of the new size", b.TotalSectors, totalSectors)
	}

	bm, err := v.readBitmap(dev)
	if err != nil {
		return err
	}

	if last := lastSetBit(bm, b.TotalClusters()); last >= clusterCount {
		return errors.Wrapf(ErrInUse, "cluster %v is in use, the volume would end at cluster %v", last, clusterCount)
	}

	bitmapChange, err := v.shrinkBitmap(bm, clusterCount)
	if err != nil {
		return err
	}

	if err := v.shrinkBadClus(clusterCount); err != nil {
		return err
	}

	flags := v.flagsOff
	binary.LittleEndian.PutUint16(v.volumeRec.buf[flags:], binary.LittleEndian.Uint16(v.volumeRec.buf[flags:])|volumeFlagDirty)

	// all changes were made in memory; write them
	for _, w := range []func() error{
		bitmapChange.write(dev),
		func() error { return v.bitmap.write(dev, b) },
		func() error { return v.badClus.write(dev, b) },
		func() error { return v.volumeRec.write(dev, b) },
		func() error { return v.writeBoot(dev, totalSectors) },
	} {
		if err := w(); err != nil {
			return err
		}
	}

	return nil
}

// bitmapWrite is the new content of $Bitmap and where it goes.
type bitmapWrite struct {
	data []byte
	runs []run
	size int64
}

func (w *bitmapWrite) write(dev Device) func() error {
	return func() error {
		data := w.data

		for _, r := range w.runs {
			n := min(int64(len(data)), r.length*clusterSizeOf(w))
			if n == 0 {
				break
			}

			if _, err := dev.WriteAt(data[:n], r.lcn*clusterSizeOf(w)); err != nil {
				return errors.Wrap(err, "unable to write $Bitmap")
			}

			data = data[n:]
		}

		return nil
	}
}

// clusterSizeOf returns the cluster size a bitmap write was made for: its
// allocated size divided by its clusters.
func clusterSizeOf(w *bitmapWrite) int64 {
	return w.size / clusters(w.runs)
}

// shrinkBitmap cuts $Bitmap to clusterCount clusters as Windows does: its size
// is the bytes those clusters need rounded up to 8, the bits past the last
// cluster are set, and clusters of $Bitmap itself that it no longer needs are
// freed.
func (v *volume) shrinkBitmap(bm []byte, clusterCount int64) (*bitmapWrite, error) {
	cs := v.boot.ClusterSize

	nr, err := v.bitmap.nonResident(v.bitmapData)
	if err != nil {
		return nil, err
	}

	if err := v.checkRuns("$Bitmap", nr.runs); err != nil {
		return nil, err
	}

	if nr.startVCN != 0 || (nr.lastVCN+1)*cs != nr.allocated || clusters(nr.runs)*cs != nr.allocated {
		return nil, errors.Errorf("$Bitmap is not described by a single attribute (clusters %v-%v, %v bytes allocated)", nr.startVCN, nr.lastVCN, nr.allocated)
	}

	size := ((clusterCount+7)/8 + bitmapRounding - 1) / bitmapRounding * bitmapRounding //nolint:mnd
	allocClusters := (size + cs - 1) / cs

	data := make([]byte, size)
	copy(data, bm)

	// the clusters of $Bitmap past its new allocation are free
	for _, r := range freedRuns(nr.runs, allocClusters) {
		if r.lcn+r.length > clusterCount {
			return nil, errors.Errorf("$Bitmap has clusters %v-%v, past the new end of the volume at %v", r.lcn, r.lcn+r.length-1, clusterCount)
		}

		for c := r.lcn; c < r.lcn+r.length; c++ {
			data[c/8] &^= 1 << (c % 8) //nolint:mnd
		}
	}

	for c := clusterCount; c < size*8; c++ {
		data[c/8] |= 1 << (c % 8) //nolint:mnd
	}

	runs := truncateRuns(nr.runs, allocClusters)

	if err := v.bitmap.setNonResident(v.bitmapData, nr, runs, allocClusters*cs, size, size); err != nil {
		return nil, errors.Wrap(err, "$Bitmap")
	}

	return &bitmapWrite{data: data, runs: runs, size: allocClusters * cs}, nil
}

// freedRuns returns the runs past the first n clusters.
func freedRuns(runs []run, n int64) []run {
	var out []run

	for _, r := range runs {
		if n >= r.length {
			n -= r.length
			continue
		}

		if !r.sparse {
			out = append(out, run{length: r.length - n, lcn: r.lcn + n})
		}

		n = 0
	}

	return out
}

// shrinkBadClus cuts $BadClus:$Bad, a sparse stream as long as the volume
// that lists the bad clusters, to clusterCount clusters.
func (v *volume) shrinkBadClus(clusterCount int64) error {
	cs := v.boot.ClusterSize

	nr, err := v.badClus.nonResident(v.badData)
	if err != nil {
		return err
	}

	if err := v.checkRuns("$BadClus", nr.runs); err != nil {
		return err
	}

	if nr.startVCN != 0 || clusters(nr.runs) != nr.lastVCN+1 || nr.lastVCN+1 < clusterCount {
		return errors.Errorf("$BadClus covers clusters %v-%v with runs of %v clusters, the volume will have %v", nr.startVCN, nr.lastVCN, clusters(nr.runs), clusterCount)
	}

	for _, r := range freedRuns(nr.runs, clusterCount) {
		if !r.sparse {
			return errors.Errorf("$BadClus lists bad clusters past the new end of the volume, at cluster %v", r.lcn)
		}
	}

	runs := truncateRuns(nr.runs, clusterCount)
	size := clusterCount * cs

	if err := v.badClus.setNonResident(v.badData, nr, runs, size, size, min(nr.initialized, size)); err != nil {
		return errors.Wrap(err, "$BadClus")
	}

	return nil
}

// setNonResident changes the sizes and runs of a non-resident attribute that
// starts at cluster 0, in place. The new runs are as many as the old or
// fewer and no longer, so their encoding fits where the old one was.
func (r *record) setNonResident(a attribute, nr *nonResident, runs []run, allocated, dataSize, initialized int64) error {
	b := r.buf[a.offset : a.offset+a.length]
	le := binary.LittleEndian
	runsOff := int(le.Uint16(b[nrRunsOffset:]))

	enc := encodeRuns(runs)
	if runsOff+len(enc) > nr.runsEnd {
		return errors.Errorf("the new run list is %v bytes, longer than the %v of the old one", len(enc), nr.runsEnd-runsOff)
	}

	copy(b[runsOff:], enc)
	clear(b[runsOff+len(enc) : nr.runsEnd])

	le.PutUint64(b[nrLastVCN:], uint64(clusters(runs)-1)) //nolint:gosec
	le.PutUint64(b[nrAllocated:], uint64(allocated))      //nolint:gosec
	le.PutUint64(b[nrDataSize:], uint64(dataSize))        //nolint:gosec
	le.PutUint64(b[nrInitialized:], uint64(initialized))  //nolint:gosec

	// the size on disk of a compressed or sparse stream: its clusters that
	// are not sparse
	if a.flags&(attrFlagCompressed|attrFlagSparse) != 0 && a.length >= nonResidentHeaderSize+8 {
		var stored int64

		for _, run := range runs {
			if !run.sparse {
				stored += run.length
			}
		}

		le.PutUint64(b[nrCompressed:], uint64(stored*(allocated/clusters(runs)))) //nolint:gosec
	}

	return nil
}

// writeBoot writes the boot sector with the new size, and its backup in the
// last sector of the partition.
func (v *volume) writeBoot(dev Device, totalSectors int64) error {
	sector := append([]byte(nil), v.bootSector...)
	binary.LittleEndian.PutUint64(sector[0x28:], uint64(totalSectors)) //nolint:gosec

	// a boot sector longer than 512 bytes keeps the rest of its sector
	full := make([]byte, v.boot.BytesPerSector)
	if _, err := dev.ReadAt(full, 0); err != nil {
		return errors.Wrap(err, "unable to read the boot sector")
	}

	copy(full, sector)

	if _, err := dev.WriteAt(full, totalSectors*v.boot.BytesPerSector); err != nil {
		return errors.Wrap(err, "unable to write the backup boot sector")
	}

	if _, err := dev.WriteAt(full, 0); err != nil {
		return errors.Wrap(err, "unable to write the boot sector")
	}

	return nil
}

// MinPartitionLength returns the shortest partition that the NTFS volume can
// be shrunk to fill, given its allocation bitmap: one that holds every
// allocated cluster, plus the backup boot sector.
func MinPartitionLength(bitmap []byte, totalClusters, clusterSize, sectorSize int64) int64 {
	last := lastSetBit(bitmap, totalClusters)

	return (last+1)*clusterSize + sectorSize
}

// bootHiddenSectors is where an NTFS boot sector holds the sector its
// partition starts at, which it calls hidden sectors.
const bootHiddenSectors = 0x1C

// SetHiddenSectors sets the starting sector of the volume's partition in its
// boot sector and backup boot sector, as Windows does when it moves a
// partition. It writes nothing when the boot sector already has it, or is
// not an NTFS boot sector it can read: it returns only I/O errors.
func SetHiddenSectors(device Device, hidden uint32) error {
	dev := &aligned{dev: device, unit: maxAlignment}

	sector := make([]byte, bootSectorSize)
	if _, err := dev.ReadAt(sector, 0); err != nil {
		return errors.Wrap(err, "unable to read the boot sector")
	}

	b, err := ParseBoot(sector)
	if err != nil {
		return nil //nolint:nilerr // not a boot sector to update
	}

	if binary.LittleEndian.Uint32(sector[bootHiddenSectors:]) == hidden {
		return nil
	}

	dev.unit = b.BytesPerSector

	// the backup first, as writeBoot does, so that the boot sector changes last
	for _, off := range []int64{b.TotalSectors * b.BytesPerSector, 0} {
		full := make([]byte, b.BytesPerSector)
		if _, err := dev.ReadAt(full, off); err != nil {
			return errors.Wrapf(err, "unable to read the boot sector at %v", off)
		}

		// a volume without its backup boot sector keeps it that way
		if _, err := ParseBoot(full); err != nil {
			continue
		}

		binary.LittleEndian.PutUint32(full[bootHiddenSectors:], hidden)

		if _, err := dev.WriteAt(full, off); err != nil {
			return errors.Wrapf(err, "unable to write the boot sector at %v", off)
		}
	}

	return nil
}

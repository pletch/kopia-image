package ntfsresize

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"strings"

	"github.com/pkg/errors"
)

// Info is what a shrink reads and changes of a volume.
type Info struct {
	Boot Boot

	// BackupBootMatches reports whether the sector after the volume holds a
	// copy of the boot sector.
	BackupBootMatches bool

	// Dirty reports whether the volume is marked for checking.
	Dirty bool

	// MFTFirstRecords is the number of MFT records in the first extent of
	// the MFT.
	MFTFirstRecords int64

	Bitmap  StreamInfo // $Bitmap
	BadClus StreamInfo // $BadClus:$Bad

	// BitmapTail is the last 8 bytes of $Bitmap, whose bits past the last
	// cluster tell how NTFS fills them.
	BitmapTail []byte

	// LastUsedCluster is the highest allocated cluster in $Bitmap, or -1.
	LastUsedCluster int64
}

// StreamInfo is the header of a non-resident stream.
type StreamInfo struct {
	Flags       uint16
	LastVCN     int64
	Allocated   int64
	DataSize    int64
	Initialized int64
	Compressed  int64 // only for compressed or sparse streams
	Runs        string
}

func (s StreamInfo) String() string {
	return fmt.Sprintf("flags %#x lastVCN %v allocated %v size %v initialized %v compressed %v runs %v",
		s.Flags, s.LastVCN, s.Allocated, s.DataSize, s.Initialized, s.Compressed, s.Runs)
}

// Inspect reads the metadata that a shrink depends on.
func Inspect(device io.ReaderAt) (*Info, error) {
	dev := &aligned{dev: readOnly{device}}

	v, err := open(dev)
	if err != nil {
		return nil, err
	}

	info := &Info{Boot: *v.boot, Dirty: v.dirty(), MFTFirstRecords: v.mftFirstRecords}

	backup := make([]byte, bootSectorSize)
	if _, err := dev.ReadAt(backup, v.boot.TotalSectors*v.boot.BytesPerSector); err == nil {
		info.BackupBootMatches = bytes.Equal(backup, v.bootSector)
	}

	if info.Bitmap, err = streamInfo(v.bitmap, v.bitmapData); err != nil {
		return nil, err
	}

	if info.BadClus, err = streamInfo(v.badClus, v.badData); err != nil {
		return nil, err
	}

	bm, err := v.readBitmap(dev)
	if err != nil {
		return nil, err
	}

	info.BitmapTail = bm[max(0, len(bm)-8):] //nolint:mnd
	info.LastUsedCluster = lastSetBit(bm, v.boot.TotalClusters())

	return info, nil
}

func streamInfo(r *record, a attribute) (StreamInfo, error) {
	nr, err := r.nonResident(a)
	if err != nil {
		return StreamInfo{}, err
	}

	s := StreamInfo{
		Flags:       a.flags,
		LastVCN:     nr.lastVCN,
		Allocated:   nr.allocated,
		DataSize:    nr.dataSize,
		Initialized: nr.initialized,
	}

	if a.flags&(attrFlagCompressed|attrFlagSparse) != 0 && a.length >= nonResidentHeaderSize+8 {
		s.Compressed = int64(binary.LittleEndian.Uint64(r.buf[a.offset+nrCompressed:])) //nolint:gosec
	}

	var parts []string

	for _, run := range nr.runs {
		if run.sparse {
			parts = append(parts, fmt.Sprintf("sparse:%v", run.length))
		} else {
			parts = append(parts, fmt.Sprintf("%v@%v", run.length, run.lcn))
		}
	}

	s.Runs = strings.Join(parts, " ")

	return s, nil
}

// readOnly is a device whose writes fail.
type readOnly struct{ io.ReaderAt }

func (readOnly) WriteAt([]byte, int64) (int, error) {
	return 0, errors.New("read-only")
}

// lastSetBit returns the highest set bit below n, or -1.
func lastSetBit(bm []byte, n int64) int64 {
	for i := min(n, int64(len(bm))*8) - 1; i >= 0; i-- { //nolint:mnd
		if bm[i/8]&(1<<(i%8)) != 0 { //nolint:mnd
			return i
		}
	}

	return -1
}

// volume is an opened NTFS volume: its boot sector and the records a shrink
// changes.
type volume struct {
	boot       *Boot
	bootSector []byte

	mftFirstRecords int64

	volumeRec  *record
	volumeInfo attribute

	bitmap     *record
	bitmapData attribute

	badClus *record
	badData attribute
}

// open reads the boot sector and the records a shrink changes, and makes dev
// read and write in sectors of the volume from then on.
func open(dev *aligned) (*volume, error) {
	dev.unit = maxAlignment

	b, sector, err := readBoot(dev)
	if err != nil {
		return nil, err
	}

	dev.unit = b.BytesPerSector

	v := &volume{boot: b, bootSector: sector}
	base := b.MFTCluster * b.ClusterSize

	mft, err := readRecord(dev, b, base, recordMFT)
	if err != nil {
		return nil, err
	}

	if err := v.checkFirstExtent(mft); err != nil {
		return nil, err
	}

	if v.volumeRec, err = readRecord(dev, b, base, recordVolume); err != nil {
		return nil, err
	}

	if v.volumeInfo, err = v.volumeRec.find(attrVolumeInformation, ""); err != nil {
		return nil, err
	}

	if v.volumeInfo.nonResident {
		return nil, errors.New("$Volume information is not resident")
	}

	if v.bitmap, err = readRecord(dev, b, base, recordBitmap); err != nil {
		return nil, err
	}

	if v.bitmapData, err = v.bitmap.find(attrData, ""); err != nil {
		return nil, err
	}

	if v.badClus, err = readRecord(dev, b, base, recordBadClus); err != nil {
		return nil, err
	}

	if v.badData, err = v.badClus.find(attrData, "$Bad"); err != nil {
		return nil, err
	}

	return v, nil
}

// checkFirstExtent checks that the records a shrink changes lie in the first
// extent of the MFT, where they are found without reading the MFT's run list
// further.
func (v *volume) checkFirstExtent(mft *record) error {
	a, err := mft.find(attrData, "")
	if err != nil {
		return err
	}

	nr, err := mft.nonResident(a)
	if err != nil {
		return err
	}

	if nr.startVCN != 0 || len(nr.runs) == 0 || nr.runs[0].sparse || nr.runs[0].lcn != v.boot.MFTCluster {
		return errors.New("the MFT does not start at the cluster the boot sector says")
	}

	v.mftFirstRecords = nr.runs[0].length * v.boot.ClusterSize / v.boot.RecordSize

	if v.mftFirstRecords <= recordBadClus {
		return errors.Errorf("the first extent of the MFT holds only %v records", v.mftFirstRecords)
	}

	return nil
}

func (v *volume) volumeFlagsOffset() int {
	a := v.volumeInfo
	valueOff := int(binary.LittleEndian.Uint16(v.volumeRec.buf[a.offset+0x14:]))

	// VOLUME_INFORMATION: 8 reserved bytes, major and minor version, flags
	return a.offset + valueOff + 0x0A //nolint:mnd
}

func (v *volume) dirty() bool {
	return binary.LittleEndian.Uint16(v.volumeRec.buf[v.volumeFlagsOffset():])&volumeFlagDirty != 0
}

// readBitmap reads the data of $Bitmap.
func (v *volume) readBitmap(dev io.ReaderAt) ([]byte, error) {
	nr, err := v.bitmap.nonResident(v.bitmapData)
	if err != nil {
		return nil, err
	}

	if nr.dataSize <= 0 || nr.dataSize > nr.allocated || nr.dataSize > clusters(nr.runs)*v.boot.ClusterSize {
		return nil, errors.Errorf("$Bitmap is %v bytes, with %v allocated", nr.dataSize, nr.allocated)
	}

	out := make([]byte, 0, nr.dataSize)

	for _, r := range nr.runs {
		if int64(len(out)) >= nr.dataSize {
			break
		}

		if r.sparse {
			return nil, errors.New("$Bitmap is sparse")
		}

		buf := make([]byte, min(r.length*v.boot.ClusterSize, nr.dataSize-int64(len(out))))
		if _, err := dev.ReadAt(buf, r.lcn*v.boot.ClusterSize); err != nil {
			return nil, errors.Wrap(err, "unable to read $Bitmap")
		}

		out = append(out, buf...)
	}

	return out, nil
}

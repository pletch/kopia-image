package imagerestore

import (
	"errors"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	pkgerrors "github.com/pkg/errors"
	"golang.org/x/sys/windows"

	"github.com/kopia/kopia/snapshot/image"
	"github.com/kopia/kopia/snapshot/image/blockdev"
	"github.com/kopia/kopia/snapshot/image/disksource"
)

const (
	ioctlDiskUpdateProperties = 0x00070140
	fsctlLockVolume           = 0x00090018
	fsctlDismountVolume       = 0x00090020
	maxPhysicalDrives         = 64
)

// deviceTarget is a whole Windows physical drive. Opening it fails if any of
// its volumes has a file system or mount point, because Windows would block
// writes to those volumes and they may be in use.
type deviceTarget struct {
	*os.File

	diskNumber int
	size       int64
	sectorSize int64

	// lockedVolumes are handles of the disk's volumes, locked and dismounted
	// for as long as the target is open.
	lockedVolumes []windows.Handle
}

// openDevice opens a physical drive. When readOnly, the drive is only read
// and its volumes stay mounted. See deviceOptions for partial restores.
func openDevice(path string, opt deviceOptions) (Target, error) {
	readOnly := opt.readOnly

	number, ok := strings.CutPrefix(strings.ToLower(path), `\\.\physicaldrive`)
	if !ok {
		return nil, pkgerrors.Errorf("%v is not a physical drive; use \\\\.\\PhysicalDriveN or a file path", path)
	}

	n, err := parseDiskNumber(number)
	if err != nil {
		return nil, pkgerrors.Wrapf(err, "invalid physical drive %v", path)
	}

	volumes, err := disksource.VolumesOnDisk(n)
	if err != nil {
		return nil, pkgerrors.Wrapf(err, "unable to list the volumes on %v", path)
	}

	volumes, err = volumesToLock(path, volumes, opt)
	if err != nil {
		return nil, err
	}

	access := uint32(windows.GENERIC_READ | windows.GENERIC_WRITE)
	if readOnly {
		access = windows.GENERIC_READ
	}

	h, err := windows.CreateFile(windows.StringToUTF16Ptr(path), access,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_WRITE_THROUGH, 0)
	if err != nil {
		return nil, pkgerrors.Wrapf(err, "unable to open %v", path)
	}

	d := &deviceTarget{File: os.NewFile(uintptr(h), path), diskNumber: int(n)}

	if !readOnly {
		if err := d.lockVolumes(volumes); err != nil {
			return nil, errors.Join(err, d.Close())
		}
	}

	// reading the size through a separate read-only handle reuses blockdev
	rd, err := blockdev.Open(path)
	if err != nil {
		return nil, errors.Join(err, d.File.Close())
	}

	d.size = rd.Size()
	d.sectorSize, err = blockdev.SectorSize(rd)

	if err = errors.Join(err, rd.Close()); err != nil {
		return nil, errors.Join(err, d.File.Close())
	}

	return d, nil
}

// volumesToLock returns the volumes of a disk that opening it locks and
// dismounts. For a whole-disk restore that is every volume, none of which may
// have a file system or mount point. For a partial restore it is the volumes
// of the selected partitions, which may have one but must not hold a path to
// keep; the disk's other volumes stay as they are.
func volumesToLock(path string, volumes []disksource.VolumeInfo, opt deviceOptions) ([]disksource.VolumeInfo, error) {
	if len(opt.partitions) == 0 {
		for _, v := range volumes {
			if len(v.MountPoints) > 0 || (v.FileSystem != "unformatted" && v.FileSystem != "RAW") {
				return nil, pkgerrors.Errorf("%v holds the volume %v (%v, mounted at %v); remove its partitions first, e.g. with diskpart \"clean\"", path, v.GUIDPath, v.FileSystem, v.MountPoints)
			}
		}

		return volumes, nil
	}

	var out []disksource.VolumeInfo

	for _, v := range volumes {
		if !slices.ContainsFunc(opt.partitions, func(p image.Partition) bool { return p.Offset == v.Offset && p.Length == v.Length }) {
			continue
		}

		holder := DiskInfo{Volumes: []DiskVolume{{MountPoints: v.MountPoints}}}
		for _, k := range opt.keepPaths {
			if k != "" && holder.HasMountPoint(k) {
				return nil, pkgerrors.Errorf("the volume at %v holds %v; it cannot be restored while in use", strings.Join(v.MountPoints, ", "), k)
			}
		}

		out = append(out, v)
	}

	return out, nil
}

// parseDiskNumber parses the decimal digits after \\.\PhysicalDrive.
func parseDiskNumber(s string) (uint32, error) {
	if s == "" {
		return 0, pkgerrors.New("missing disk number")
	}

	// ParseUint accepts only digits and rejects numbers that do not fit
	n, err := strconv.ParseUint(s, 10, 32)
	if err != nil {
		return 0, pkgerrors.Errorf("invalid disk number %q", s)
	}

	return uint32(n), nil
}

func (d *deviceTarget) Path() string      { return d.Name() }
func (d *deviceTarget) Size() int64       { return d.size }
func (d *deviceTarget) SectorSize() int64 { return d.sectorSize }
func (d *deviceTarget) IsDevice() bool    { return true }
func (d *deviceTarget) IsZeroed() bool    { return false }

// writeRetryDelays are the waits before writing again to a disk that did not
// complete a write in time, such as a USB disk whose adapter stalled: about a
// minute in all before the restore fails.
//
//nolint:gochecknoglobals
var writeRetryDelays = []time.Duration{
	time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 15 * time.Second, 30 * time.Second,
}

// WriteAt writes to the drive, and again while the drive does not complete
// writes in time: USB adapters stall now and then under sustained writes.
func (d *deviceTarget) WriteAt(b []byte, off int64) (int, error) {
	return retryWrite(d.File.WriteAt, b, off, isTransientWriteError, writeRetryDelays, time.Sleep)
}

// isTransientWriteError reports whether a write failed because the drive was
// slow or briefly unavailable, rather than refused it.
func isTransientWriteError(err error) bool {
	for _, e := range []windows.Errno{
		windows.ERROR_SEM_TIMEOUT, // the drive did not complete the write in time
		windows.ERROR_IO_DEVICE,   // the drive's adapter reset
		windows.ERROR_NOT_READY,
		windows.ERROR_BUSY,
	} {
		if errors.Is(err, e) {
			return true
		}
	}

	return false
}

func (d *deviceTarget) Zero(off, length int64) error {
	return writeZeros(d, off, length)
}

func (d *deviceTarget) Sync() error {
	return pkgerrors.Wrapf(d.File.Sync(), "unable to sync %v", d.Name())
}

// RereadPartitions makes Windows reread the partition table and mount the
// restored volumes.
func (d *deviceTarget) RereadPartitions() error {
	sc, err := d.SyscallConn()
	if err != nil {
		return pkgerrors.Wrap(err, "unable to get the disk handle")
	}

	var ioErr error

	if err := sc.Control(func(fd uintptr) {
		_, ioErr = blockdev.IoControl(windows.Handle(fd), ioctlDiskUpdateProperties, nil, nil)
	}); err != nil {
		return pkgerrors.Wrap(err, "unable to use the disk handle")
	}

	return pkgerrors.Wrapf(ioErr, "unable to update the partitions of %v", d.Name())
}

func (d *deviceTarget) Close() error {
	err := pkgerrors.Wrapf(d.File.Close(), "unable to close %v", d.Name())

	// closing a locked volume's handle unlocks it
	for _, h := range d.lockedVolumes {
		err = errors.Join(err, windows.CloseHandle(h))
	}

	d.lockedVolumes = nil

	return err
}

// lockVolumes locks and dismounts the disk's volumes, all without a file
// system, until the target is closed. Windows mounts a volume as soon as it
// recognizes a file system on it, such as the NTFS boot sector at the start of
// a partition being restored, and then denies raw writes to it; anything that
// touches the volume mid-restore, like Explorer, antivirus or the Virtual Disk
// Service, would trigger that. A locked volume cannot be mounted.
func (d *deviceTarget) lockVolumes(volumes []disksource.VolumeInfo) error {
	for _, v := range volumes {
		path := strings.TrimSuffix(v.GUIDPath, `\`)

		h, err := windows.CreateFile(windows.StringToUTF16Ptr(path),
			windows.GENERIC_READ|windows.GENERIC_WRITE,
			windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
			nil, windows.OPEN_EXISTING, 0, 0)
		if err != nil {
			return pkgerrors.Wrapf(err, "unable to open volume %v", v.GUIDPath)
		}

		d.lockedVolumes = append(d.lockedVolumes, h)

		if _, err := blockdev.IoControl(h, fsctlLockVolume, nil, nil); err != nil {
			return pkgerrors.Wrapf(err, "unable to lock volume %v; is it in use?", v.GUIDPath)
		}

		if _, err := blockdev.IoControl(h, fsctlDismountVolume, nil, nil); err != nil {
			return pkgerrors.Wrapf(err, "unable to dismount volume %v", v.GUIDPath)
		}
	}

	return nil
}

// otherDiskGUIDs returns the GPT disk GUIDs of the other physical drives.
func otherDiskGUIDs(target Target) (map[string]string, error) {
	targetNumber := -1
	if d, ok := target.(*deviceTarget); ok {
		targetNumber = d.diskNumber
	}

	out := map[string]string{}

	for n := range maxPhysicalDrives {
		if n == targetNumber {
			continue
		}

		l, err := blockdev.ReadLayout(uint32(n))
		if err != nil {
			continue
		}

		out[l.DiskGUID] = blockdev.DiskPath(uint32(n))
	}

	return out, nil
}

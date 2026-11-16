package blockdev

import (
	"strconv"
	"strings"

	"github.com/pkg/errors"
	"golang.org/x/sys/windows"

	"github.com/kopia/kopia/snapshot/image"
)

const maxVolumeDiskExtents = 32

// DiskPath returns the device path of a physical disk.
func DiskPath(diskNumber uint32) string {
	return `\\.\PhysicalDrive` + strconv.FormatUint(uint64(diskNumber), 10)
}

// VolumeDevicePath returns the device path of a volume given as a drive letter
// ("C:", "C:\"), a volume GUID path (`\\?\Volume{...}\`) or the folder a
// volume is mounted at.
func VolumeDevicePath(volume string) (string, error) {
	v := strings.TrimSuffix(volume, `\`)

	switch {
	case len(v) == 2 && v[1] == ':' && isASCIILetter(v[0]):
		return `\\.\` + strings.ToUpper(v), nil
	case strings.HasPrefix(v, `\\?\Volume{`) && strings.HasSuffix(v, "}"):
		return v, nil
	}

	p, err := windows.UTF16PtrFromString(v + `\`)
	if err != nil {
		return "", errors.Wrapf(err, "invalid volume %q", volume)
	}

	buf := make([]uint16, windows.MAX_PATH)

	if err := windows.GetVolumeNameForVolumeMountPoint(p, &buf[0], uint32(len(buf))); err != nil { //nolint:gosec
		return "", errors.Wrapf(err, "%q is not a drive letter, volume GUID path or volume mount point", volume)
	}

	return strings.TrimSuffix(windows.UTF16ToString(buf), `\`), nil
}

func isASCIILetter(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// VolumeDiskExtents returns the parts of physical disks that hold a volume. A
// basic volume has exactly one extent.
func VolumeDiskExtents(volume string) ([]DiskExtent, error) {
	path, err := VolumeDevicePath(volume)
	if err != nil {
		return nil, err
	}

	h, err := openForQuery(path)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(h) //nolint:errcheck

	out := make([]byte, diskExtentsHeaderSize+maxVolumeDiskExtents*diskExtentSize)

	n, err := IoControl(h, ioctlVolumeGetVolumeDiskExtents, nil, out)
	if err != nil {
		return nil, errors.Wrapf(err, "unable to get disk extents of %q", volume)
	}

	return decodeDiskExtents(out[:n])
}

// ReadLayout reads the GPT partition table of a physical disk. It returns an
// error wrapping image.ErrUnsupportedLayout for MBR and raw disks, dynamic
// disks and Storage Spaces pool disks.
func ReadLayout(diskNumber uint32) (*image.Layout, error) {
	path := DiskPath(diskNumber)

	h, err := openForQuery(path)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(h) //nolint:errcheck

	var geom [diskGeometryExMinSize + 64]byte

	n, err := IoControl(h, ioctlDiskGetDriveGeometryEx, nil, geom[:])
	if err != nil {
		return nil, errors.Wrapf(err, "unable to get geometry of %q", path)
	}

	sectorSize, diskSize, err := decodeGeometry(geom[:n])
	if err != nil {
		return nil, err
	}

	out := make([]byte, driveLayoutHeaderSize+initialDriveLayoutPartitionsCount*partitionInfoSize)

	for {
		n, err = IoControl(h, ioctlDiskGetDriveLayoutEx, nil, out)
		if !errors.Is(err, windows.ERROR_INSUFFICIENT_BUFFER) {
			break
		}

		out = make([]byte, 2*len(out)) //nolint:mnd
	}

	if err != nil {
		return nil, errors.Wrapf(err, "unable to get drive layout of %q", path)
	}

	diskGUID, partitions, err := decodeDriveLayout(out[:n])
	if err != nil {
		return nil, errors.Wrapf(err, "disk %v", diskNumber)
	}

	l, err := buildLayout(diskGUID, sectorSize, diskSize, partitions)

	return l, errors.Wrapf(err, "disk %v", diskNumber)
}

// openForQuery opens a device for I/O control queries without read access.
func openForQuery(path string) (windows.Handle, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, errors.Wrapf(err, "invalid device path %q", path)
	}

	h, err := windows.CreateFile(p, 0,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		return 0, errors.Wrapf(err, "unable to open %q", path)
	}

	return h, nil
}

// SectorSize returns the logical sector size of a disk device.
func SectorSize(d *Device) (int64, error) {
	var geom [diskGeometryExMinSize + 64]byte

	n, err := d.IoControl(ioctlDiskGetDriveGeometryEx, nil, geom[:])
	if err != nil {
		return 0, errors.Wrapf(err, "unable to get the geometry of %q", d.Path())
	}

	sectorSize, _, err := decodeGeometry(geom[:n])

	return sectorSize, err
}

const ioctlStorageQueryProperty = 0x002D1400

// DescribeDisk returns the model and bus of a physical disk.
func DescribeDisk(diskNumber uint32) (DiskDescriptor, error) {
	h, err := openForQuery(DiskPath(diskNumber))
	if err != nil {
		return DiskDescriptor{}, err
	}
	defer windows.CloseHandle(h) //nolint:errcheck

	// STORAGE_PROPERTY_QUERY for StorageDeviceProperty, PropertyStandardQuery: all zero
	var (
		query [12]byte
		out   [1024]byte
	)

	n, err := IoControl(h, ioctlStorageQueryProperty, query[:], out[:])
	if err != nil {
		return DiskDescriptor{}, errors.Wrapf(err, "unable to query disk %v", diskNumber)
	}

	return decodeDeviceDescriptor(out[:n])
}

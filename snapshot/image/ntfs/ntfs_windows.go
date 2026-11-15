package ntfs

import (
	"encoding/binary"

	"github.com/pkg/errors"
	"golang.org/x/sys/windows"

	"github.com/kopia/kopia/snapshot/image"
	"github.com/kopia/kopia/snapshot/image/blockdev"
)

const (
	fsctlGetVolumeBitmap = 0x0009006f

	bitmapChunkBytes     = 8 << 20
	retrievalBufferBytes = 64 << 10
	dirInfoBufferBytes   = 64 << 10

	systemVolumeInformation = "System Volume Information"
)

// unfrozenRootFiles are files in the volume root whose clusters volsnap does
// not copy on write, so their contents can change while a shadow copy is read.
// Windows recreates or reinitializes all of them at boot.
var unfrozenRootFiles = []string{"pagefile.sys", "hiberfil.sys", "swapfile.sys"}

// Volume describes an NTFS volume or shadow copy as needed to image it.
type Volume struct {
	Data VolumeData

	// Size is the size of the volume device in bytes. It can exceed
	// TotalClusters*ClusterSize; the tail holds the backup boot sector.
	Size int64

	// Bitmap is the cluster allocation bitmap; bit i%8 of byte i/8 is set when
	// cluster i is in use.
	Bitmap []byte

	// UnfrozenFiles lists the files, relative to the volume root, whose
	// clusters the shadow copy does not freeze. UnfrozenExtents are their
	// allocated byte extents.
	UnfrozenFiles   []string
	UnfrozenExtents []image.Extent
}

// FreeExtents returns the byte extents of the free clusters.
func (v *Volume) FreeExtents() ([]image.Extent, error) {
	free, err := image.FreeExtents(v.Bitmap, v.Data.TotalClusters, v.Data.ClusterSize)

	return free, errors.Wrap(err, "invalid volume bitmap")
}

// ZeroMask returns the extents to read as zeros when imaging the volume: the
// free clusters and the clusters of unfrozen files.
func (v *Volume) ZeroMask() ([]image.Extent, error) {
	free, err := v.FreeExtents()
	if err != nil {
		return nil, err
	}

	mask, err := image.NormalizeExtents(append(free, v.UnfrozenExtents...))

	return mask, errors.Wrap(err, "invalid unfrozen file extents")
}

// Probe reads the geometry, allocation bitmap and unfrozen file extents of
// the NTFS volume open as dev, normally a shadow copy device. Files are looked
// up below dev.Path(). The caller must have enabled the backup privilege, see
// EnableBackupPrivilege.
func Probe(dev *blockdev.Device) (*Volume, error) {
	vd, err := GetVolumeData(dev)
	if err != nil {
		return nil, err
	}

	if vd.TotalClusters > dev.Size()/vd.ClusterSize {
		return nil, errors.Errorf("volume has %v clusters of %v bytes, more than the device size %v", vd.TotalClusters, vd.ClusterSize, dev.Size())
	}

	bitmap, err := GetVolumeBitmap(dev, vd.TotalClusters)
	if err != nil {
		return nil, err
	}

	files, extents, err := UnfrozenFileExtents(dev.Path()+`\`, vd.ClusterSize)
	if err != nil {
		return nil, err
	}

	return &Volume{
		Data:            vd,
		Size:            dev.Size(),
		Bitmap:          bitmap,
		UnfrozenFiles:   files,
		UnfrozenExtents: extents,
	}, nil
}

// GetVolumeData returns the geometry of an NTFS volume. It fails for volumes
// with other file systems.
func GetVolumeData(dev *blockdev.Device) (VolumeData, error) {
	out := make([]byte, 2*volumeDataSize) //nolint:mnd

	n, err := dev.IoControl(windows.FSCTL_GET_NTFS_VOLUME_DATA, nil, out)
	if err != nil {
		return VolumeData{}, errors.Wrapf(err, "unable to get NTFS volume data of %q (not an NTFS volume?)", dev.Path())
	}

	vd, err := decodeVolumeData(out[:n])

	return vd, errors.Wrapf(err, "volume %q", dev.Path())
}

// GetVolumeBitmap returns the allocation bitmap of the first totalClusters
// clusters of a volume.
func GetVolumeBitmap(dev *blockdev.Device, totalClusters int64) ([]byte, error) {
	bitmap := make([]byte, (totalClusters+7)/8) //nolint:mnd
	out := make([]byte, bitmapHeaderSize+bitmapChunkBytes)

	var in [8]byte

	for lcn := int64(0); lcn < totalClusters; {
		binary.LittleEndian.PutUint64(in[:], uint64(lcn))

		n, ioErr := dev.IoControl(fsctlGetVolumeBitmap, in[:], out)
		if ioErr != nil && !errors.Is(ioErr, windows.ERROR_MORE_DATA) {
			return nil, errors.Wrapf(ioErr, "unable to get volume bitmap of %q at cluster %v", dev.Path(), lcn)
		}

		start, chunk, err := decodeBitmapChunk(out[:n])
		if err != nil {
			return nil, err
		}

		if start > lcn || start/8 >= int64(len(bitmap)) {
			return nil, errors.Errorf("volume bitmap of %q starts at cluster %v, requested %v", dev.Path(), start, lcn)
		}

		copied := int64(copy(bitmap[start/8:], chunk))
		if start+copied*8 <= lcn {
			return nil, errors.Errorf("volume bitmap of %q made no progress at cluster %v", dev.Path(), lcn)
		}

		lcn = start + copied*8 //nolint:mnd

		if ioErr == nil && lcn < totalClusters {
			return nil, errors.Errorf("volume bitmap of %q ends at cluster %v of %v", dev.Path(), lcn, totalClusters)
		}
	}

	return bitmap, nil
}

// UnfrozenFileExtents returns the files below root (a volume root path ending
// in a backslash) whose clusters volume shadow copies do not freeze, and their
// allocated byte extents. Files that do not exist are skipped. Listing System
// Volume Information requires the backup privilege.
func UnfrozenFileExtents(root string, clusterSize int64) ([]string, []image.Extent, error) {
	candidates := append([]string(nil), unfrozenRootFiles...)

	names, err := listDirectory(root + systemVolumeInformation)
	if err != nil {
		return nil, nil, err
	}

	for _, name := range names {
		if isShadowStorageFile(name) {
			candidates = append(candidates, systemVolumeInformation+`\`+name)
		}
	}

	var (
		files   []string
		extents []image.Extent
	)

	for _, rel := range candidates {
		fileExtents, err := FileExtents(root+rel, clusterSize)
		if isNotFound(err) {
			continue
		}

		if err != nil {
			return nil, nil, err
		}

		files = append(files, rel)
		extents = append(extents, fileExtents...)
	}

	return files, extents, nil
}

// FileExtents returns the allocated byte extents of a file's default data
// stream. Resident and empty files have none.
func FileExtents(path string, clusterSize int64) ([]image.Extent, error) {
	h, err := openFile(path, windows.FILE_READ_ATTRIBUTES)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(h) //nolint:errcheck

	var (
		extents []image.Extent
		in      [8]byte
		vcn     int64
	)

	out := make([]byte, retrievalBufferBytes)

	for {
		binary.LittleEndian.PutUint64(in[:], uint64(vcn))

		n, ioErr := blockdev.IoControl(h, windows.FSCTL_GET_RETRIEVAL_POINTERS, in[:], out)
		if errors.Is(ioErr, windows.ERROR_HANDLE_EOF) {
			return extents, nil
		}

		if ioErr != nil && !errors.Is(ioErr, windows.ERROR_MORE_DATA) {
			return nil, errors.Wrapf(ioErr, "unable to get extents of %q", path)
		}

		extents, vcn, err = appendRetrievalExtents(extents, out[:n], clusterSize)
		if err != nil {
			return nil, errors.Wrapf(err, "file %q", path)
		}

		if ioErr == nil {
			return extents, nil
		}
	}
}

// listDirectory returns the names in a directory, or nil if it does not exist.
func listDirectory(path string) ([]string, error) {
	h, err := openFile(path, windows.FILE_LIST_DIRECTORY)
	if isNotFound(err) {
		return nil, nil
	}

	if err != nil {
		return nil, err
	}

	defer windows.CloseHandle(h) //nolint:errcheck

	var names []string

	buf := make([]byte, dirInfoBufferBytes)

	for {
		err := windows.GetFileInformationByHandleEx(h, windows.FileIdBothDirectoryInfo, &buf[0], uint32(len(buf))) //nolint:gosec
		if errors.Is(err, windows.ERROR_NO_MORE_FILES) {
			return names, nil
		}

		if err != nil {
			return nil, errors.Wrapf(err, "unable to list %q", path)
		}

		batch, err := decodeDirectoryNames(buf)
		if err != nil {
			return nil, errors.Wrapf(err, "directory %q", path)
		}

		names = append(names, batch...)
	}
}

func openFile(path string, access uint32) (windows.Handle, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, errors.Wrapf(err, "invalid path %q", path)
	}

	h, err := windows.CreateFile(p, access,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return 0, errors.Wrapf(err, "unable to open %q", path)
	}

	return h, nil
}

func isNotFound(err error) bool {
	return errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND)
}

// EnableBackupPrivilege enables SeBackupPrivilege for the process, which
// listing System Volume Information needs when not running as SYSTEM.
func EnableBackupPrivilege() error {
	var tok windows.Token

	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_ADJUST_PRIVILEGES|windows.TOKEN_QUERY, &tok); err != nil {
		return errors.Wrap(err, "unable to open process token")
	}
	defer tok.Close() //nolint:errcheck

	name, err := windows.UTF16PtrFromString("SeBackupPrivilege")
	if err != nil {
		return errors.Wrap(err, "invalid privilege name")
	}

	var luid windows.LUID

	if err := windows.LookupPrivilegeValue(nil, name, &luid); err != nil {
		return errors.Wrap(err, "unable to look up SeBackupPrivilege")
	}

	tp := windows.Tokenprivileges{
		PrivilegeCount: 1,
		Privileges:     [1]windows.LUIDAndAttributes{{Luid: luid, Attributes: windows.SE_PRIVILEGE_ENABLED}},
	}

	return errors.Wrap(windows.AdjustTokenPrivileges(tok, false, &tp, 0, nil, nil), "unable to enable SeBackupPrivilege")
}

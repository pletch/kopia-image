package localfs

import (
	"os"
	"runtime"
	"strings"

	"golang.org/x/sys/windows"

	"github.com/kopia/kopia/fs"
)

const isWindows = runtime.GOOS == "windows"

func platformSpecificOwnerInfo(_ os.FileInfo) fs.OwnerInfo {
	return fs.OwnerInfo{}
}

func platformSpecificDeviceInfo(_ os.FileInfo) fs.DeviceInfo {
	return fs.DeviceInfo{}
}

// Direct Windows volume paths (e.g. Shadow Copy) require a trailing separator.
func trailingSeparator(fsd *filesystemDirectory) string {
	// is fsd a Windows VSS Volume and has no trailing separator?
	if isWindows &&
		fsd.prefix == `\\?\GLOBALROOT\Device\` &&
		strings.HasPrefix(fsd.Name(), "HarddiskVolumeShadowCopy") &&
		!strings.HasSuffix(fsd.Name(), separatorStr) {
		return separatorStr
	}

	return ""
}

// preserveTimeWord in both halves of a FILETIME tells SetFileTime to leave
// that time of a file unchanged by the handle's further operations.
const preserveTimeWord = 0xFFFFFFFF

var preserveTime = windows.Filetime{LowDateTime: preserveTimeWord, HighDateTime: preserveTimeWord}

// openPreservingAccessTime opens a file or directory for reading without
// updating its last access time, when NTFS keeps those up to date: a
// snapshot then reads a folder without NTFS writing to the disk, which would
// change the blocks that disk images of it are made of, every time. It opens
// the handle with the right to set the times of the file and tells it to
// leave the last access time alone; where that right is not granted, or the
// volume is read-only such as a shadow copy, it opens the file as os.Open
// does.
func openPreservingAccessTime(path string, isDir bool) (*os.File, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return os.Open(path) //nolint:gosec,wrapcheck
	}

	access := uint32(windows.GENERIC_READ | windows.FILE_WRITE_ATTRIBUTES)
	flags := uint32(windows.FILE_ATTRIBUTE_NORMAL)

	if isDir {
		access = windows.FILE_LIST_DIRECTORY | windows.FILE_READ_ATTRIBUTES | windows.SYNCHRONIZE | windows.FILE_WRITE_ATTRIBUTES
		flags = windows.FILE_FLAG_BACKUP_SEMANTICS
	}

	h, err := windows.CreateFile(p, access,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, flags, 0)
	if err != nil {
		return os.Open(path) //nolint:gosec,wrapcheck
	}

	// best effort: the file is read either way
	_ = windows.SetFileTime(h, nil, &preserveTime, nil)

	return os.NewFile(uintptr(h), path), nil
}

//go:build windows && !cgo

package winfspmount

import (
	"context"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/pkg/errors"
	"github.com/winfsp/cgofuse/fuse"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"

	"github.com/kopia/kopia/fs"
)

const (
	// maxLabelLength is the longest volume label WinFsp accepts.
	maxLabelLength = 32

	// unmountTimeout bounds the wait for WinFsp to finish after an unmount.
	unmountTimeout = 30 * time.Second
)

// ErrNotInstalled is returned by Mount when WinFsp is not installed.
var ErrNotInstalled = errors.New("WinFsp is not installed")

// Available reports whether WinFsp is installed. The WinFsp FUSE layer stops
// the program when its DLL cannot be found, so Mount must not be called
// without it.
func Available() bool {
	return dllPath() != ""
}

// dllPath returns the path of the WinFsp DLL, found as the FUSE layer finds
// it: on the DLL search path or in the bin folder of the WinFsp installation.
func dllPath() string {
	const dllName = "winfsp-x64.dll"

	if p, err := windows.LoadLibraryEx(dllName, 0, windows.LOAD_LIBRARY_SEARCH_SYSTEM32|windows.LOAD_LIBRARY_SEARCH_APPLICATION_DIR); err == nil {
		windows.FreeLibrary(p) //nolint:errcheck

		return dllName
	}

	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `Software\WinFsp`, registry.QUERY_VALUE|registry.WOW64_32KEY)
	if err != nil {
		return ""
	}
	defer k.Close() //nolint:errcheck

	dir, _, err := k.GetStringValue("InstallDir")
	if err != nil || dir == "" {
		return ""
	}

	p := filepath.Join(dir, "bin", dllName)

	h, err := windows.LoadLibraryEx(p, 0, windows.LOAD_WITH_ALTERED_SEARCH_PATH)
	if err != nil {
		return ""
	}

	windows.FreeLibrary(h) //nolint:errcheck

	return p
}

// Controller is a mounted directory.
type Controller struct {
	host  *fuse.FileSystemHost
	fs    *kopiaFS
	drive string
	done  chan struct{}
}

// Mount mounts a directory as a read-only drive. driveLetter is a drive such
// as "K:", or "*" for the last free letter. label names the drive in Explorer.
func Mount(ctx context.Context, root fs.Directory, driveLetter, label string) (*Controller, error) {
	if !Available() {
		return nil, ErrNotInstalled
	}

	drive := driveLetter
	if drive == "*" {
		var err error
		if drive, err = freeDriveLetter(); err != nil {
			return nil, err
		}
	}

	kfs := newKopiaFS(context.WithoutCancel(ctx), root)

	host := fuse.NewFileSystemHost(kfs)
	host.SetCapCaseInsensitive(true)
	host.SetCapReaddirPlus(true)

	c := &Controller{host: host, fs: kfs, drive: drive, done: make(chan struct{})}

	mountResult := make(chan bool, 1)

	go func() {
		defer close(c.done)

		mountResult <- host.Mount(drive, mountOptions(label))
	}()

	select {
	case <-kfs.mounted:
		log(ctx).Debugf("mounted on %v with WinFsp", drive)
		return c, nil

	case <-mountResult:
		return nil, errors.Errorf("WinFsp could not mount %v", drive)
	}
}

// mountOptions returns the WinFsp FUSE options: the drive belongs to the
// current user, and since a snapshot never changes, Windows may cache file
// information and data for as long as the drive is mounted.
func mountOptions(label string) []string {
	return []string{
		"-o", "uid=-1,gid=-1,umask=0222",
		"-o", "FileInfoTimeout=-1,DirInfoTimeout=-1,VolumeInfoTimeout=-1",
		"-o", "volname=" + volumeLabel(label),
		"-o", "ExactFileSystemName=Kopia",
	}
}

// volumeLabel shortens a label to what WinFsp accepts and removes the
// characters that would end the option.
func volumeLabel(label string) string {
	label = strings.Map(func(r rune) rune {
		if r == ',' || r == '\\' || r < ' ' {
			return '_'
		}

		return r
	}, label)

	if u := utf16.Encode([]rune(label)); len(u) > maxLabelLength {
		label = string(utf16.Decode(u[:maxLabelLength]))
	}

	return label
}

// freeDriveLetter returns the last drive letter that is neither in use nor
// remembered for a network drive, as 'net use *' would choose.
func freeDriveLetter() (string, error) {
	used, err := windows.GetLogicalDrives()
	if err != nil {
		return "", errors.Wrap(err, "unable to list drives")
	}

	for d := 'Z'; d >= 'D'; d-- {
		if used&(1<<(d-'A')) != 0 || rememberedNetworkDrive(d) {
			continue
		}

		return string(d) + ":", nil
	}

	return "", errors.New("no free drive letter")
}

// rememberedNetworkDrive reports whether a disconnected network drive keeps
// the letter: Windows would reconnect it there.
func rememberedNetworkDrive(d rune) bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, `Network\`+string(d), registry.QUERY_VALUE)
	if err != nil {
		return false
	}

	k.Close() //nolint:errcheck

	return true
}

// MountPath returns the drive, such as "K:".
func (c *Controller) MountPath() string {
	return c.drive
}

// Done is closed when the drive has been unmounted.
func (c *Controller) Done() <-chan struct{} {
	return c.done
}

// Unmount removes the drive.
func (c *Controller) Unmount(_ context.Context) error {
	if !c.host.Unmount() {
		return errors.Errorf("unable to unmount %v", c.drive)
	}

	select {
	case <-c.done:
		return nil
	case <-time.After(unmountTimeout):
		return errors.Errorf("timed out unmounting %v", c.drive)
	}
}

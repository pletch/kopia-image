package recoverymedia

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"unsafe"

	ole "github.com/go-ole/go-ole"
	"github.com/go-ole/go-ole/oleutil"
	pkgerrors "github.com/pkg/errors"
	"golang.org/x/sys/windows"
)

// IMAPI2FS values, from imapi2fs.h.
const (
	fsiISO9660 = 1
	fsiJoliet  = 2
	fsiUDF     = 4

	platformEFI   = 0xef
	emulationNone = 0

	// vtable slots: IUnknown has 3 methods and IDispatch 4 more.
	bootOptionsAssignBootImage = 15
	streamRead                 = 3

	isoReadBuffer = 1 << 20
)

var errNoISOStream = errors.New("the ISO image has no stream")

//nolint:gochecknoglobals
var (
	iidIBootOptions = ole.NewGUID("{2C941FD4-975B-59BE-A960-9A2A262853A5}")

	procSHCreateStreamOnFileEx = windows.NewLazySystemDLL("shlwapi.dll").NewProc("SHCreateStreamOnFileEx")
)

// WriteISO writes the media to a new ISO file, to write to a USB stick with
// a tool such as Rufus, which copies its files to a FAT32 stick that boots
// like media from WriteUSB. It uses the image mastering API of Windows: the
// files go in ISO 9660, Joliet and UDF file systems, and the El Torito boot
// catalog names the EFI boot image that the media holds, without emulation.
//
// The ISO does not boot as a DVD or in a virtual machine: the DVD boot
// image comes from Windows, whose copy is not updated with the boot
// manager, and an older one fails to start a newer recovery system.
func WriteISO(ctx context.Context, mediaDir, path string) (err error) {
	path, err = filepath.Abs(path)
	if err != nil {
		return pkgerrors.Wrap(err, "invalid path")
	}

	bootImage := filepath.Join(mediaDir, MediaEFIBootImage)
	if _, err := os.Stat(bootImage); err != nil {
		return pkgerrors.Wrap(err, "the media lacks the EFI boot image of a DVD")
	}

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec,mnd // the path the administrator gave
	if err != nil {
		return pkgerrors.Wrap(err, "unable to create the ISO file")
	}

	defer func() {
		err = errors.Join(err, f.Close())
		if err != nil {
			os.Remove(path) //nolint:errcheck
		}
	}()

	// COM objects belong to the thread that created them
	runtime.LockOSThread()

	defer runtime.UnlockOSThread()

	if err := ole.CoInitializeEx(0, ole.COINIT_MULTITHREADED); err != nil {
		var oerr *ole.OleError
		// S_FALSE: already initialized on this thread
		if !errors.As(err, &oerr) || oerr.Code() != 1 {
			return pkgerrors.Wrap(err, "unable to initialize COM")
		}
	}

	defer ole.CoUninitialize()

	stream, err := buildISOStream(mediaDir, bootImage)
	if err != nil {
		return err
	}
	defer stream.Release()

	return copyStream(ctx, f, stream)
}

// buildISOStream returns the stream of the ISO file's bytes, which the
// image mastering API produces as it is read.
func buildISOStream(mediaDir, bootImage string) (*ole.IUnknown, error) {
	fsi, err := createDispatch("IMAPI2FS.MsftFileSystemImage")
	if err != nil {
		return nil, err
	}
	defer fsi.Release()

	boot, err := createDispatch("IMAPI2FS.BootOptions")
	if err != nil {
		return nil, err
	}
	defer boot.Release()

	if err := assignBootImage(boot, bootImage); err != nil {
		return nil, err
	}

	for _, p := range []struct {
		obj   *ole.IDispatch
		name  string
		value any
	}{
		{boot, "PlatformId", int32(platformEFI)},
		{boot, "Emulation", int32(emulationNone)},
		{fsi, "FileSystemsToCreate", int32(fsiISO9660 | fsiJoliet | fsiUDF)},
		{fsi, "VolumeName", VolumeLabel},
		// no limit on the size
		{fsi, "FreeMediaBlocks", int32(0)},
		{fsi, "BootImageOptions", boot},
	} {
		if err := putProperty(p.obj, p.name, p.value); err != nil {
			return nil, err
		}
	}

	root, err := oleutil.GetProperty(fsi, "Root")
	if err != nil {
		return nil, pkgerrors.Wrap(err, "unable to read the root of the ISO file system")
	}
	defer root.Clear() //nolint:errcheck

	if _, err := oleutil.CallMethod(root.ToIDispatch(), "AddTree", mediaDir, false); err != nil {
		return nil, pkgerrors.Wrapf(err, "unable to add %v to the ISO file system", mediaDir)
	}

	result, err := oleutil.CallMethod(fsi, "CreateResultImage")
	if err != nil {
		return nil, pkgerrors.Wrap(err, "unable to create the ISO image")
	}
	defer result.Clear() //nolint:errcheck

	sv, err := oleutil.GetProperty(result.ToIDispatch(), "ImageStream")
	if err != nil {
		return nil, pkgerrors.Wrap(err, "unable to read the ISO image")
	}

	stream := sv.ToIUnknown()
	if stream == nil {
		sv.Clear() //nolint:errcheck
		return nil, errNoISOStream
	}

	// the caller releases the stream; the variant's reference is handed over
	return stream, nil
}

func createDispatch(progID string) (*ole.IDispatch, error) {
	unk, err := oleutil.CreateObject(progID)
	if err != nil {
		return nil, pkgerrors.Wrapf(err, "unable to create %v; the Windows image mastering API is missing", progID)
	}
	defer unk.Release()

	d, err := unk.QueryInterface(ole.IID_IDispatch)

	return d, pkgerrors.Wrapf(err, "unable to use %v", progID)
}

func putProperty(obj *ole.IDispatch, name string, value any) error {
	_, err := oleutil.PutProperty(obj, name, value)

	return pkgerrors.Wrapf(err, "unable to set %v of the ISO image", name)
}

// assignBootImage gives the boot options the boot image as a stream. It
// calls the method through the vtable: go-ole passes no IUnknown arguments
// through IDispatch.
func assignBootImage(boot *ole.IDispatch, path string) error {
	stream, err := openFileStream(path)
	if err != nil {
		return err
	}
	defer stream.Release()

	opts, err := boot.QueryInterface(iidIBootOptions)
	if err != nil {
		return pkgerrors.Wrap(err, "unable to use the boot options")
	}
	defer opts.Release()

	if hr := vtableCall(&opts.IUnknown, bootOptionsAssignBootImage, uintptr(unsafe.Pointer(stream))); hr != 0 { //nolint:gosec // COM call
		return pkgerrors.Wrapf(ole.NewError(hr), "unable to use %v as the boot image", path)
	}

	return nil
}

func openFileStream(path string) (*ole.IUnknown, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, pkgerrors.Wrap(err, "invalid path")
	}

	var stream *ole.IUnknown

	const stgmReadShareDenyWrite = 0x20

	//nolint:gosec // Windows API call
	hr, _, _ := procSHCreateStreamOnFileEx.Call(uintptr(unsafe.Pointer(p)), stgmReadShareDenyWrite, 0, 0, 0, uintptr(unsafe.Pointer(&stream)))
	if hr != 0 {
		return nil, pkgerrors.Wrapf(ole.NewError(hr), "unable to open %v", path)
	}

	return stream, nil
}

// copyStream copies an IStream to a file.
func copyStream(ctx context.Context, w io.Writer, stream *ole.IUnknown) error {
	buf := make([]byte, isoReadBuffer)

	for {
		if err := ctx.Err(); err != nil {
			return err //nolint:wrapcheck
		}

		var n uint32

		hr := vtableCall(stream, streamRead, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)), uintptr(unsafe.Pointer(&n))) //nolint:gosec // COM call
		// S_FALSE: fewer bytes than asked, at the end
		if hr != 0 && hr != 1 {
			return pkgerrors.Wrap(ole.NewError(hr), "unable to read the ISO image")
		}

		if n > 0 {
			if _, err := w.Write(buf[:n]); err != nil {
				return pkgerrors.Wrap(err, "unable to write the ISO file")
			}
		}

		if n == 0 || hr == 1 {
			return nil
		}
	}
}

// vtableCall calls a COM method by its slot in the object's vtable and
// returns its HRESULT.
func vtableCall(obj *ole.IUnknown, slot int, args ...uintptr) uintptr {
	vtbl := unsafe.Slice((*uintptr)(unsafe.Pointer(obj.RawVTable)), slot+1) //nolint:gosec // COM vtable

	hr, _, _ := syscall.SyscallN(vtbl[slot], append([]uintptr{uintptr(unsafe.Pointer(obj))}, args...)...) //nolint:gosec // COM call

	return hr
}

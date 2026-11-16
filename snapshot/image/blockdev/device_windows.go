package blockdev

import (
	stderrors "errors"
	"io"
	"os"
	"sync"
	"unsafe"

	"github.com/pkg/errors"
	"golang.org/x/sys/windows"
)

// I/O control codes not defined in golang.org/x/sys/windows.
const (
	ioctlDiskGetDriveLayoutEx         = 0x00070050
	ioctlDiskGetDriveGeometryEx       = 0x000700a0
	ioctlDiskGetLengthInfo            = 0x0007405c
	ioctlVolumeGetVolumeDiskExtents   = 0x00560000
	fsctlAllowExtendedDasdIO          = 0x00090083
	bounceBufferSize                  = 4 << 20
	initialDriveLayoutPartitionsCount = 128
)

// Device is a read-only handle to a disk, partition, volume or volume shadow
// copy, opened for unbuffered reads so that the image does not pass through
// the file system cache.
type Device struct {
	f    *os.File
	path string
	size int64

	bounce sync.Pool // of *alignedBuffer
}

type alignedBuffer struct {
	b []byte // unbufferedAlignment-aligned, bounceBufferSize long
}

// Open opens a device such as `\\.\PhysicalDrive0`, `\\.\C:` or
// `\\?\GLOBALROOT\Device\HarddiskVolumeShadowCopy3` for reading.
func Open(path string) (*Device, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, errors.Wrapf(err, "invalid device path %q", path)
	}

	h, err := windows.CreateFile(p,
		windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_NO_BUFFERING,
		0)
	if err != nil {
		return nil, errors.Wrapf(err, "unable to open device %q", path)
	}

	d := &Device{f: os.NewFile(uintptr(h), path), path: path}

	var out [lengthInfoSize]byte

	n, err := d.IoControl(ioctlDiskGetLengthInfo, nil, out[:])
	if err != nil {
		return nil, stderrors.Join(errors.Wrapf(err, "unable to get length of %q", path), d.Close())
	}

	if d.size, err = decodeLength(out[:n]); err != nil {
		return nil, stderrors.Join(err, d.Close())
	}

	// A file system on the device rejects reads past its last cluster, where
	// NTFS keeps the backup boot sector, unless the handle opts out of its
	// boundary checks. Devices without a mounted file system do not support
	// the request and need no opt-out, so its failure is ignored.
	_, _ = d.IoControl(fsctlAllowExtendedDasdIO, nil, nil)

	return d, nil
}

// Path returns the path the device was opened with.
func (d *Device) Path() string {
	return d.path
}

// Size returns the size of the device in bytes.
func (d *Device) Size() int64 {
	return d.size
}

// Close closes the device.
func (d *Device) Close() error {
	return errors.Wrapf(d.f.Close(), "unable to close %q", d.path)
}

// IoControl sends an I/O control request to the device and returns the
// number of bytes written to out. On windows.ERROR_MORE_DATA it returns the
// error together with the number of bytes written.
func (d *Device) IoControl(code uint32, in, out []byte) (int, error) {
	sc, err := d.f.SyscallConn()
	if err != nil {
		return 0, errors.Wrap(err, "unable to get device handle")
	}

	var (
		n     int
		ioErr error
	)

	if err := sc.Control(func(fd uintptr) {
		n, ioErr = IoControl(windows.Handle(fd), code, in, out)
	}); err != nil {
		return 0, errors.Wrap(err, "unable to use device handle")
	}

	return n, ioErr
}

// IoControl sends an I/O control request to an open handle; see
// Device.IoControl.
func IoControl(h windows.Handle, code uint32, in, out []byte) (int, error) {
	var inPtr, outPtr *byte

	if len(in) > 0 {
		inPtr = &in[0]
	}

	if len(out) > 0 {
		outPtr = &out[0]
	}

	var n uint32

	err := windows.DeviceIoControl(h, code, inPtr, uint32(len(in)), outPtr, uint32(len(out)), &n, nil) //nolint:gosec

	return int(n), err
}

// ReadAt implements io.ReaderAt. Unbuffered reads must be sector-aligned in
// offset, length and buffer address; requests that are not are served through
// an aligned bounce buffer. Reads are clamped to the device size.
func (d *Device) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.Errorf("invalid offset %v", off)
	}

	if off >= d.size {
		return 0, io.EOF
	}

	want := min(int64(len(p)), d.size-off)

	var (
		n   int
		err error
	)

	if isAligned(p[:want], off) {
		n, err = d.readFull(p[:want], off)
	} else {
		n, err = d.readBounced(p[:want], off)
	}

	if err != nil {
		return n, err
	}

	if int64(n) < int64(len(p)) {
		return n, io.EOF
	}

	return n, nil
}

func isAligned(p []byte, off int64) bool {
	if len(p) == 0 {
		return true
	}

	return off%unbufferedAlignment == 0 &&
		len(p)%unbufferedAlignment == 0 &&
		uintptr(unsafe.Pointer(&p[0]))%unbufferedAlignment == 0 //nolint:gosec
}

// readFull reads exactly len(p) bytes at an aligned offset, or ends at the
// device size, which is a multiple of the sector size.
func (d *Device) readFull(p []byte, off int64) (int, error) {
	var total int

	for total < len(p) {
		n, err := d.f.ReadAt(p[total:], off+int64(total))
		total += n

		if n > 0 && err == nil {
			continue
		}

		if errors.Is(err, io.EOF) && total == len(p) {
			break
		}

		if err == nil || errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}

		return total, errors.Wrapf(err, "error reading %q at offset %v", d.path, off+int64(total))
	}

	return total, nil
}

func (d *Device) readBounced(p []byte, off int64) (int, error) {
	buf := d.getBounceBuffer()
	defer d.bounce.Put(buf)

	var copied int

	for copied < len(p) {
		pos := off + int64(copied)
		alignedStart := pos - pos%unbufferedAlignment
		skip := int(pos - alignedStart)

		// the device size is a multiple of the sector size, so the clamped end stays sector-aligned
		alignedEnd := min(alignedStart+int64(len(buf.b)), d.size, roundUp(off+int64(len(p))))

		n, err := d.readFull(buf.b[:alignedEnd-alignedStart], alignedStart)
		if n > skip {
			copied += copy(p[copied:], buf.b[skip:n])
		}

		if err != nil {
			return copied, err
		}
	}

	return copied, nil
}

func roundUp(v int64) int64 {
	return (v + unbufferedAlignment - 1) / unbufferedAlignment * unbufferedAlignment
}

func (d *Device) getBounceBuffer() *alignedBuffer {
	if v, ok := d.bounce.Get().(*alignedBuffer); ok {
		return v
	}

	return &alignedBuffer{b: AlignedBuffer(bounceBufferSize)}
}

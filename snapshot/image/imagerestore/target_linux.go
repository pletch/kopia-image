package imagerestore

import (
	"errors"
	"os"
	"path/filepath"
	"unsafe"

	pkgerrors "github.com/pkg/errors"
	"golang.org/x/sys/unix"

	"github.com/kopia/kopia/snapshot/image/gpt"
)

// Block device ioctls missing from golang.org/x/sys/unix. They are all
// _IO(0x12, n), so they are derived from BLKDISCARD (_IO(0x12, 119)) to keep
// the architecture's direction bits.
const (
	blkSSZGet   = unix.BLKDISCARD - 119 + 104 // _IO(0x12, 104)
	blkRRPart   = unix.BLKDISCARD - 119 + 95  // _IO(0x12, 95)
	blkZeroOut  = unix.BLKDISCARD - 119 + 127 // _IO(0x12, 127)
	blkFlsBuf   = unix.BLKDISCARD - 119 + 97  // _IO(0x12, 97)
	sysClassBlk = "/sys/class/block"
)

// deviceTarget is a whole Linux block device opened exclusively, which fails
// if any of its partitions is mounted or otherwise in use.
type deviceTarget struct {
	*os.File

	size       int64
	sectorSize int64
}

// openDevice opens a whole disk exclusively, also when readOnly, so that a
// dry run reports a disk that is in use. Exclusive access to the whole disk
// requires every partition to be unused, also for a partial restore.
func openDevice(path string, opt deviceOptions) (Target, error) {
	readOnly := opt.readOnly

	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, pkgerrors.Wrapf(err, "unable to resolve %v", path)
	}

	if _, err := os.Stat(filepath.Join(sysClassBlk, filepath.Base(resolved), "partition")); err == nil {
		return nil, pkgerrors.Errorf("%v is a partition; the target must be a whole disk", path)
	}

	flags := os.O_RDWR
	if readOnly {
		flags = os.O_RDONLY
	}

	f, err := os.OpenFile(resolved, flags|unix.O_EXCL, 0)
	if err != nil {
		return nil, pkgerrors.Wrapf(err, "unable to open %v exclusively (is it mounted or in use?)", path)
	}

	d := &deviceTarget{File: f}

	var size uint64

	//nolint:gosec // the ioctl argument is a pointer to size
	if err := ioctlPtr(f, unix.BLKGETSIZE64, unsafe.Pointer(&size)); err != nil {
		return nil, errors.Join(pkgerrors.Wrapf(err, "unable to get the size of %v", path), f.Close())
	}

	sectorSize, err := unix.IoctlGetInt(int(f.Fd()), blkSSZGet)
	if err != nil {
		return nil, errors.Join(pkgerrors.Wrapf(err, "unable to get the sector size of %v", path), f.Close())
	}

	d.size = int64(size)
	d.sectorSize = int64(sectorSize)

	return d, nil
}

func ioctlPtr(f *os.File, req uintptr, arg unsafe.Pointer) error {
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, f.Fd(), req, uintptr(arg)); errno != 0 {
		return errno
	}

	return nil
}

func (d *deviceTarget) Path() string      { return d.Name() }
func (d *deviceTarget) Size() int64       { return d.size }
func (d *deviceTarget) SectorSize() int64 { return d.sectorSize }
func (d *deviceTarget) IsDevice() bool    { return true }
func (d *deviceTarget) IsZeroed() bool    { return false }

// Zero uses BLKZEROOUT, which lets the device zero the range without the
// data crossing the bus, and falls back to writing zeros.
func (d *deviceTarget) Zero(off, length int64) error {
	r := [2]uint64{uint64(off), uint64(length)} //nolint:gosec

	err := ioctlPtr(d.File, blkZeroOut, unsafe.Pointer(&r)) //nolint:gosec // ioctl argument
	if err == nil {
		return nil
	}

	if errors.Is(err, unix.ENOTTY) || errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.EINVAL) {
		return writeZeros(d, off, length)
	}

	return pkgerrors.Wrapf(err, "BLKZEROOUT failed on %v", d.Name())
}

func (d *deviceTarget) Sync() error {
	return pkgerrors.Wrapf(d.File.Sync(), "unable to sync %v", d.Name())
}

// dropCache drops the device's cached pages, so that what is read next comes
// from the disk.
func (d *deviceTarget) dropCache() error {
	return pkgerrors.Wrapf(ioctlPtr(d.File, blkFlsBuf, nil), "unable to drop the cache of %v", d.Name())
}

func (d *deviceTarget) RereadPartitions() error {
	return pkgerrors.Wrapf(ioctlPtr(d.File, blkRRPart, nil), "unable to reread the partitions of %v", d.Name())
}

func (d *deviceTarget) Close() error {
	return pkgerrors.Wrapf(d.File.Close(), "unable to close %v", d.Name())
}

// otherDiskGUIDs returns the GPT disk GUIDs of the other disks, by path.
func otherDiskGUIDs(target Target) (map[string]string, error) {
	resolvedTarget, _ := filepath.EvalSymlinks(target.Path())

	entries, err := os.ReadDir("/sys/block")
	if err != nil {
		return nil, pkgerrors.Wrap(err, "unable to list disks")
	}

	out := map[string]string{}

	for _, e := range entries {
		path := "/dev/" + e.Name()
		if path == resolvedTarget || !isDiskName(e.Name()) {
			continue
		}

		if g, ok := readDiskGUID(path); ok {
			out[g] = path
		}
	}

	return out, nil
}

func readDiskGUID(path string) (string, bool) {
	f, err := os.Open(path) //nolint:gosec
	if err != nil {
		return "", false
	}
	defer f.Close() //nolint:errcheck

	sectorSize, err := unix.IoctlGetInt(int(f.Fd()), blkSSZGet)
	if err != nil {
		return "", false
	}

	t, err := gpt.Read(f, int64(sectorSize))
	if err != nil {
		return "", false
	}

	return t.DiskGUID, true
}

package imagerestore

import (
	stderrors "errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/pkg/errors"

	"github.com/kopia/kopia/snapshot/image"
	"github.com/kopia/kopia/snapshot/image/blockdev"
)

// Target is a disk, or a file standing in for one, that an image is restored to.
//
//nolint:interfacebloat // one device abstraction, implemented per platform
type Target interface {
	io.WriterAt
	io.ReaderAt

	// Path is the path the target was opened with.
	Path() string

	// Size and SectorSize are in bytes.
	Size() int64
	SectorSize() int64

	// IsDevice reports whether the target is a block device rather than a file.
	IsDevice() bool

	// IsZeroed reports whether every byte reads as zero, as for a new file.
	IsZeroed() bool

	// Zero zeroes a range, quickly where the device supports it.
	Zero(off, length int64) error

	// Sync flushes written data to stable storage.
	Sync() error

	// RereadPartitions tells the operating system that the partition table
	// changed. It does nothing for files.
	RereadPartitions() error

	Close() error
}

// OpenTarget opens a restore target for writing. A path that does not exist
// is created as a sparse file of createSize bytes with the given sector size;
// an existing regular file is used as is; anything else is opened as a block
// device, which must be a whole disk. Opening a Windows disk locks and
// dismounts its volumes until the target is closed.
func OpenTarget(path string, createSize, sectorSize int64) (Target, error) {
	if isWindowsDevicePath(path) {
		return openDevice(path, deviceOptions{})
	}

	fi, err := os.Stat(path)

	switch {
	case errors.Is(err, os.ErrNotExist):
		return createFileTarget(path, createSize, sectorSize)
	case err != nil:
		return nil, errors.Wrapf(err, "unable to examine %v", path)
	case fi.Mode().IsRegular():
		return openFileTarget(path, fi.Size(), sectorSize, false)
	default:
		return openDevice(path, deviceOptions{})
	}
}

// deviceOptions configure openDevice.
type deviceOptions struct {
	readOnly bool

	// partitions, when set, are the only partitions a partial restore
	// writes. On Windows their volumes are locked and dismounted, also when
	// they have a file system, and the disk's other volumes stay mounted.
	partitions []image.Partition

	// keepPaths are paths that must survive, such as the repository: a
	// volume among partitions that holds one is refused.
	keepPaths []string
}

// OpenPartitionsTarget opens an existing restore target to restore volumes
// into some of the partitions of its partition table, as a partial restore
// does. On Windows the volumes of those partitions are locked and dismounted
// until the target is closed, and the others stay mounted; a volume that
// holds one of keepPaths, such as the repository or Windows, is refused. On
// Linux, as for OpenTarget, no partition of the disk may be in use. When
// readOnly, as for a dry run, nothing is locked or written.
func OpenPartitionsTarget(path string, sectorSize int64, readOnly bool, partitions []image.Partition, keepPaths ...string) (Target, error) {
	opt := deviceOptions{readOnly: readOnly, partitions: partitions, keepPaths: keepPaths}

	if isWindowsDevicePath(path) {
		return openDevice(path, opt)
	}

	fi, err := os.Stat(path)

	switch {
	case err != nil:
		return nil, errors.Wrapf(err, "unable to examine %v", path)
	case fi.Mode().IsRegular():
		return openFileTarget(path, fi.Size(), sectorSize, readOnly)
	default:
		return openDevice(path, opt)
	}
}

// OpenTargetReadOnly opens an existing restore target to examine it, as a
// dry run does: writes fail and nothing about the target changes. The target
// is checked like OpenTarget checks it, so a disk that is in use is refused.
func OpenTargetReadOnly(path string, sectorSize int64) (Target, error) {
	if isWindowsDevicePath(path) {
		return openDevice(path, deviceOptions{readOnly: true})
	}

	fi, err := os.Stat(path)

	switch {
	case err != nil:
		return nil, errors.Wrapf(err, "unable to examine %v", path)
	case fi.Mode().IsRegular():
		return openFileTarget(path, fi.Size(), sectorSize, true)
	default:
		return openDevice(path, deviceOptions{readOnly: true})
	}
}

// CheckNewTargetPath checks that a file can be created at path to hold a disk
// image. It refuses paths under /dev: a mistyped device name there would
// become a regular file in memory-backed storage and fill the memory with the
// image instead of writing a disk.
func CheckNewTargetPath(path string) error {
	if runtime.GOOS == "windows" {
		return nil
	}

	abs, err := filepath.Abs(path)
	if err != nil {
		return errors.Wrapf(err, "unable to resolve %v", path)
	}

	// a folder that links into /dev leads there too
	if dir, err := filepath.EvalSymlinks(filepath.Dir(abs)); err == nil {
		abs = filepath.Join(dir, filepath.Base(abs))
	}

	if abs == devDir || strings.HasPrefix(abs, devDir+"/") {
		return errors.Errorf("%v does not exist; check the device name (a file is not created under %v)", path, devDir)
	}

	return nil
}

const devDir = "/dev"

func isWindowsDevicePath(path string) bool {
	return strings.HasPrefix(path, `\\.\`) || strings.HasPrefix(path, `\\?\`)
}

// dataProbeSize is how much of each end of a target HasData reads.
const dataProbeSize = 1 << 20

// HasData reports whether a target holds anything in its first or last MiB:
// a partition table, its backup, or a file system, encrypted volume, LVM or
// RAID signature on the whole disk. Restoring to it would overwrite that.
func HasData(t Target) (bool, error) {
	n := min(t.Size(), dataProbeSize) / t.SectorSize() * t.SectorSize()
	buf := blockdev.AlignedBuffer(int(n))

	for _, off := range []int64{0, t.Size() - n} {
		if _, err := t.ReadAt(buf, off); err != nil && !errors.Is(err, io.EOF) {
			return false, errors.Wrapf(err, "unable to read %v bytes at offset %v", n, off)
		}

		if slices.ContainsFunc(buf, func(b byte) bool { return b != 0 }) {
			return true, nil
		}
	}

	return false, nil
}

// fileTarget is a regular file standing in for a disk.
type fileTarget struct {
	*os.File

	size       int64
	sectorSize int64
	zeroed     bool
}

func createFileTarget(path string, size, sectorSize int64) (Target, error) {
	if err := CheckNewTargetPath(path); err != nil {
		return nil, err
	}

	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec,mnd
	if err != nil {
		return nil, errors.Wrapf(err, "unable to create %v", path)
	}

	if err := f.Truncate(size); err != nil {
		return nil, stderrors.Join(errors.Wrapf(err, "unable to size %v", path), f.Close())
	}

	return &fileTarget{File: f, size: size, sectorSize: sectorSize, zeroed: true}, nil
}

func openFileTarget(path string, size, sectorSize int64, readOnly bool) (Target, error) {
	flags := os.O_RDWR
	if readOnly {
		flags = os.O_RDONLY
	}

	f, err := os.OpenFile(path, flags, 0) //nolint:gosec
	if err != nil {
		return nil, errors.Wrapf(err, "unable to open %v", path)
	}

	return &fileTarget{File: f, size: size, sectorSize: sectorSize, zeroed: size == 0}, nil
}

func (f *fileTarget) Path() string            { return f.Name() }
func (f *fileTarget) Size() int64             { return f.size }
func (f *fileTarget) SectorSize() int64       { return f.sectorSize }
func (f *fileTarget) IsDevice() bool          { return false }
func (f *fileTarget) IsZeroed() bool          { return f.zeroed }
func (f *fileTarget) RereadPartitions() error { return nil }

func (f *fileTarget) Zero(off, length int64) error {
	return writeZeros(f, off, length)
}

func (f *fileTarget) Sync() error {
	return errors.Wrapf(f.File.Sync(), "unable to sync %v", f.Name())
}

func (f *fileTarget) Close() error {
	return errors.Wrapf(f.File.Close(), "unable to close %v", f.Name())
}

// writeZeros writes zeros to a range in sector-multiple writes.
func writeZeros(w io.WriterAt, off, length int64) error {
	buf := make([]byte, min(length, zeroWriteSize))

	for done := int64(0); done < length; {
		n := min(int64(len(buf)), length-done)

		if _, err := w.WriteAt(buf[:n], off+done); err != nil {
			return errors.Wrapf(err, "unable to write zeros at %v", off+done)
		}

		done += n
	}

	return nil
}

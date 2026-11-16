package imagerestore

import (
	"bytes"
	stderrors "errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/pkg/errors"
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
		return openDevice(path, false)
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
		return openDevice(path, false)
	}
}

// OpenTargetReadOnly opens an existing restore target to examine it, as a
// dry run does: writes fail and nothing about the target changes. The target
// is checked like OpenTarget checks it, so a disk that is in use is refused.
func OpenTargetReadOnly(path string, sectorSize int64) (Target, error) {
	if isWindowsDevicePath(path) {
		return openDevice(path, true)
	}

	fi, err := os.Stat(path)

	switch {
	case err != nil:
		return nil, errors.Wrapf(err, "unable to examine %v", path)
	case fi.Mode().IsRegular():
		return openFileTarget(path, fi.Size(), sectorSize, true)
	default:
		return openDevice(path, true)
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

	if abs == devDir || strings.HasPrefix(abs, devDir+"/") {
		return errors.Errorf("%v does not exist; check the device name (a file is not created under %v)", path, devDir)
	}

	return nil
}

const devDir = "/dev"

func isWindowsDevicePath(path string) bool {
	return strings.HasPrefix(path, `\\.\`) || strings.HasPrefix(path, `\\?\`)
}

// HasPartitionTable reports whether a target holds an MBR or GPT signature,
// i.e. whether restoring to it would overwrite an existing disk layout.
func HasPartitionTable(t Target) (bool, error) {
	sector := make([]byte, t.SectorSize())

	if _, err := t.ReadAt(sector, 0); err != nil && !errors.Is(err, io.EOF) {
		return false, errors.Wrap(err, "unable to read the first sector")
	}

	if sector[510] == 0x55 && sector[511] == 0xaa {
		return true, nil
	}

	if _, err := t.ReadAt(sector, t.SectorSize()); err != nil && !errors.Is(err, io.EOF) {
		return false, errors.Wrap(err, "unable to read the second sector")
	}

	return bytes.HasPrefix(sector, []byte("EFI PART")), nil
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

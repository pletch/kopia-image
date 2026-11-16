package imagerestore

import (
	"bytes"
	stderrors "errors"
	"io"
	"os"
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

// OpenTarget opens a restore target. A path that does not exist is created
// as a sparse file of createSize bytes with the given sector size; an
// existing regular file is used as is; anything else is opened as a block
// device, which must be a whole disk.
func OpenTarget(path string, createSize, sectorSize int64) (Target, error) {
	if isWindowsDevicePath(path) {
		return openDevice(path)
	}

	fi, err := os.Stat(path)

	switch {
	case errors.Is(err, os.ErrNotExist):
		return createFileTarget(path, createSize, sectorSize)
	case err != nil:
		return nil, errors.Wrapf(err, "unable to examine %v", path)
	case fi.Mode().IsRegular():
		return openFileTarget(path, fi.Size(), sectorSize)
	default:
		return openDevice(path)
	}
}

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
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec,mnd
	if err != nil {
		return nil, errors.Wrapf(err, "unable to create %v", path)
	}

	if err := f.Truncate(size); err != nil {
		return nil, stderrors.Join(errors.Wrapf(err, "unable to size %v", path), f.Close())
	}

	return &fileTarget{File: f, size: size, sectorSize: sectorSize, zeroed: true}, nil
}

func openFileTarget(path string, size, sectorSize int64) (Target, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0) //nolint:gosec
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

package ntfsresize

import (
	"github.com/pkg/errors"
)

// maxAlignment is the unit of the reads of the boot sector, before the
// sector size is known: Windows disk and volume handles accept only whole
// sectors, which are 512 or 4096 bytes, and a volume is longer than that.
const maxAlignment = 4096

// aligned reads and writes a device in whole sectors, reading the sectors
// that a write covers only in part first. It never touches bytes outside the
// sectors that a read or write covers, so it stays within the partition.
type aligned struct {
	dev  Device
	unit int64
}

func (a *aligned) span(off int64, n int) (start, end int64) {
	start = off / a.unit * a.unit
	end = (off + int64(n) + a.unit - 1) / a.unit * a.unit

	return start, end
}

func (a *aligned) ReadAt(p []byte, off int64) (int, error) {
	start, end := a.span(off, len(p))

	buf := make([]byte, end-start)
	if _, err := a.dev.ReadAt(buf, start); err != nil {
		return 0, errors.Wrapf(err, "unable to read %v bytes at %v", len(buf), start)
	}

	return copy(p, buf[off-start:]), nil
}

func (a *aligned) WriteAt(p []byte, off int64) (int, error) {
	start, end := a.span(off, len(p))

	buf := make([]byte, end-start)
	if start != off || end != off+int64(len(p)) {
		if _, err := a.ReadAt(buf, start); err != nil {
			return 0, err
		}
	}

	copy(buf[off-start:], p)

	if _, err := a.dev.WriteAt(buf, start); err != nil {
		return 0, errors.Wrapf(err, "unable to write %v bytes at %v", len(buf), start)
	}

	return len(p), nil
}

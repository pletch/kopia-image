package repo

import (
	"context"

	"github.com/pkg/errors"
)

// NewData is how much new data a repository writer has stored so far.
// Contents that already existed in the repository are not counted.
type NewData struct {
	// Bytes is the size of the new contents before compression.
	Bytes int64

	// StoredBytes is their size as stored, compressed and encrypted; valid
	// only if StoredKnown.
	StoredBytes int64
	StoredKnown bool
}

// Since returns the new data stored between an earlier reading and d: the
// bytes before compression, and the bytes stored or nil if not known.
func (d NewData) Since(before NewData) (newBytes int64, storedBytes *int64) {
	if d.StoredKnown && !before.StoredKnown {
		// read through a repository server before it first reported its
		// totals, which were zero then
		before = NewData{StoredKnown: true}
	}

	newBytes = d.Bytes - before.Bytes

	if d.StoredKnown {
		n := d.StoredBytes - before.StoredBytes
		storedBytes = &n
	}

	return newBytes, storedBytes
}

// NewDataReporter is implemented by repository writers other than direct ones
// that count the new data they store.
type NewDataReporter interface {
	NewData() NewData
}

// NewDataOf returns how much new data w has stored so far. With a direct
// repository these are the contents added to packs, uploaded or not. Through
// a repository server it is what the server reported at the last flush, so
// callers flush before reading it, and read the starting value before
// writing anything.
func NewDataOf(w RepositoryWriter) NewData {
	if dw, ok := w.(DirectRepositoryWriter); ok {
		cm := dw.ContentManager()

		return NewData{Bytes: cm.NewBytes(), StoredBytes: cm.PackedBytes(), StoredKnown: true}
	}

	if r, ok := w.(NewDataReporter); ok {
		return r.NewData()
	}

	return NewData{}
}

// FlushForNewData flushes w if NewDataOf(w) can count what was written only
// after a flush: through a repository server, which reports its totals on a
// flush. A direct repository counts contents as they are added to packs, so
// it is not flushed here; a snapshot then writes its contents and manifest
// under a single index blob, as it would without counting new data.
func FlushForNewData(ctx context.Context, w RepositoryWriter) error {
	if _, ok := w.(DirectRepositoryWriter); ok {
		return nil
	}

	if _, ok := w.(NewDataReporter); !ok {
		// nothing to count, so nothing to flush for
		return nil
	}

	return errors.Wrap(w.Flush(ctx), "unable to flush to count new data")
}

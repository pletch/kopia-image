package repo

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

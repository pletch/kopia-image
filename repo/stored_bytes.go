package repo

// StoredBytesReporter is implemented by repository writers that know how many
// bytes of new contents they stored, compressed and encrypted.
type StoredBytesReporter interface {
	// StoredBytes returns the running total and whether it is known. A
	// client of a repository server learns it when it flushes, from servers
	// that report it.
	StoredBytes() (int64, bool)
}

// StoredBytes returns how many bytes of new contents w has stored so far,
// compressed and encrypted, and whether that is known. Contents that already
// existed in the repository are not counted. With a direct repository these
// are the contents added to packs, uploaded or not; through a repository
// server, what the server reported at the last flush, so callers flush
// before reading it.
func StoredBytes(w RepositoryWriter) (int64, bool) {
	if dw, ok := w.(DirectRepositoryWriter); ok {
		return dw.ContentManager().PackedBytes(), true
	}

	if r, ok := w.(StoredBytesReporter); ok {
		return r.StoredBytes()
	}

	return 0, false
}

package upload

import (
	"github.com/kopia/kopia/repo"
)

// NewDataCounter returns a function that reports how many bytes of new data
// the write session w has stored so far, to record in a snapshot manifest as
// its uploaded bytes. Callers take the difference between two readings.
//
// With a direct repository these are the new contents added to packs,
// compressed and encrypted, whether or not the packs were uploaded yet, so
// no flush is needed before reading. Through a repository server, they are
// what uploaded reports: the session's WriteSessionOptions.OnUpload counts
// each content as it is sent to the server.
func NewDataCounter(w repo.RepositoryWriter, uploaded func() int64) func() int64 {
	if dw, ok := w.(repo.DirectRepositoryWriter); ok {
		return dw.ContentManager().PackedBytes
	}

	return uploaded
}

//go:build !linux && !freebsd && !darwin

package filesystem

//nolint:revive
func (realOS) IsStale(err error) bool {
	return false
}

// SyncDir does nothing: Windows makes a rename durable with the file system's
// own metadata journal and can't open a directory for syncing.
func (realOS) SyncDir(_ string) error {
	return nil
}

//go:build !windows

package securedir

// Secure is not supported on this platform.
func Secure(string, Access) error { return ErrNotSupported }

// SecureFile is not supported on this platform.
func SecureFile(string) error { return ErrNotSupported }

// Describe is not supported on this platform.
func Describe(string) (string, error) { return "", ErrNotSupported }

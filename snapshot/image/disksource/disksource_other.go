//go:build !windows

package disksource

import "context"

// DefaultStatePath is not supported on this platform.
func DefaultStatePath() (string, error) {
	return "", ErrNotSupported
}

// Acquire is not supported on this platform.
func Acquire(string, func(string, ...any)) (*Guard, error) {
	return nil, ErrNotSupported
}

// DisksForVolumes is not supported on this platform.
func DisksForVolumes([]string) ([]uint32, error) {
	return nil, ErrNotSupported
}

// PrepareDisk is not supported on this platform.
func PrepareDisk(uint32) (*Plan, error) {
	return nil, ErrNotSupported
}

// OpenSession is not supported on this platform.
func OpenSession(context.Context, *Plan, *ShadowState, func(string, ...any)) (*Session, error) {
	return nil, ErrNotSupported
}

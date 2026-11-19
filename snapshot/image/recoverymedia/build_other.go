//go:build !windows

package recoverymedia

import "context"

// IsElevated reports whether the process runs as an administrator.
func IsElevated() bool { return false }

// Build is supported only on Windows.
func Build(context.Context, *BuildOptions) (*BuildResult, error) { return nil, ErrNotSupported }

// LocateWinRE is supported only on Windows.
func LocateWinRE(context.Context) (string, error) { return "", ErrNotSupported }

// WriteFolder is supported only on Windows.
func WriteFolder(string, string) error { return ErrNotSupported }

// WriteVHDX is supported only on Windows.
func WriteVHDX(context.Context, string, string, string) error { return ErrNotSupported }

// WriteISO is supported only on Windows.
func WriteISO(context.Context, string, string) error { return ErrNotSupported }

// WriteUSB is supported only on Windows.
func WriteUSB(context.Context, string, uint32, int64, string) error { return ErrNotSupported }

// FindMedia finds recovery media only in the Windows recovery environment.
func FindMedia() (string, error) { return "", nil }

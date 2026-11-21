//go:build !windows

package schedule

import "context"

// Install is not supported on this platform.
func Install(context.Context, *Task) error { return ErrNotSupported }

// Delete is not supported on this platform.
func Delete(context.Context, string) error { return ErrNotSupported }

// Run is not supported on this platform.
func Run(context.Context, string) error { return ErrNotSupported }

// Query is not supported on this platform.
func Query(context.Context, string) (map[string]string, error) { return nil, ErrNotSupported }

// ProgramDataDir is not supported on this platform.
func ProgramDataDir() (string, error) { return "", ErrNotSupported }

// ShareStatus is not supported on this platform.
func ShareStatus(context.Context, string) error { return ErrNotSupported }

// SetEnabled is not supported on this platform.
func SetEnabled(context.Context, string, bool) error { return ErrNotSupported }

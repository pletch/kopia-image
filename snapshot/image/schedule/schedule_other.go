//go:build !windows

package schedule

import "context"

// ErrTaskNotFound is returned when a task does not exist.
var ErrTaskNotFound = ErrNotSupported

// Install is not supported on this platform.
func Install(context.Context, *Task) error { return ErrNotSupported }

// Delete is not supported on this platform.
func Delete(context.Context, string) error { return ErrNotSupported }

// Run is not supported on this platform.
func Run(context.Context, string) error { return ErrNotSupported }

// Query is not supported on this platform.
func Query(context.Context, string) (map[string]string, error) { return nil, ErrNotSupported }

// SecureDirectory is not supported on this platform.
func SecureDirectory(string) error { return ErrNotSupported }

// DirectoryDACL is not supported on this platform.
func DirectoryDACL(string) (string, error) { return "", ErrNotSupported }

// ProgramDataDir is not supported on this platform.
func ProgramDataDir() (string, error) { return "", ErrNotSupported }

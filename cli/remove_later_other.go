//go:build !windows

package cli

import "github.com/pkg/errors"

// removeAtRestart is supported only on Windows, where a program that runs
// cannot be removed.
func removeAtRestart(dir string) ([]string, error) {
	return nil, errors.Errorf("unable to remove all of %v", dir)
}

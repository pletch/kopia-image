package cli

import (
	"io/fs"
	"path/filepath"
	"slices"

	"github.com/pkg/errors"
	"golang.org/x/sys/windows"
)

// removeAtRestart marks what is left of dir, after removing all that could be
// removed, for Windows to remove when it restarts. Files stay behind while a
// program uses them, such as the tasks' kopia during an image run, which
// Windows does not let anyone remove. It returns the files left. It requires
// administrator.
func removeAtRestart(dir string) ([]string, error) {
	var left []string

	err := filepath.WalkDir(dir, func(path string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		left = append(left, path)

		return nil
	})
	if err != nil {
		return nil, errors.Wrapf(err, "unable to list %v", dir)
	}

	// the contents of a folder go before the folder
	slices.Reverse(left)

	var files []string

	for _, path := range left {
		p, err := windows.UTF16PtrFromString(path)
		if err != nil {
			return nil, errors.Wrapf(err, "invalid path %v", path)
		}

		if err := windows.MoveFileEx(p, nil, windows.MOVEFILE_DELAY_UNTIL_REBOOT); err != nil {
			return nil, errors.Wrapf(err, "unable to remove %v when Windows restarts", path)
		}

		if path != dir {
			files = append(files, path)
		}
	}

	slices.Reverse(files)

	return files, nil
}

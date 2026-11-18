package imagerestore

import (
	stderrors "errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/pkg/errors"

	"github.com/kopia/kopia/snapshot/image"
)

// ErrRestoredVolumeNotFound is returned when the volume of a restored
// partition cannot be found, as for a restore to a disk image file or on a
// system that does not mount NTFS volumes.
var ErrRestoredVolumeNotFound = errors.New("the restored volume was not found")

// maxDeleteErrors bounds the errors reported by DeleteExcludedFiles.
const maxDeleteErrors = 10

// DeleteExcludedFiles deletes from a restored volume the files that its image
// read as zeros (see image.ReadExcludedFiles), so that the programs that used
// them recreate them instead of reading zeros. root is the path of the
// volume's root folder; files are relative to it, with backslash
// separators. Only regular files are deleted; files that are already gone are
// skipped. It returns the number of files deleted.
func DeleteExcludedFiles(root string, files []string) (int, error) {
	var (
		deleted int
		errs    []error
		failed  int
	)

	for _, f := range files {
		if !image.IsVolumeRelativePath(f) {
			return deleted, errors.Errorf("invalid excluded file path %q", f)
		}

		path := filepath.Join(root, filepath.FromSlash(strings.ReplaceAll(f, `\`, "/")))

		ok, err := deleteRegularFile(path)
		if err != nil {
			failed++

			if failed <= maxDeleteErrors {
				errs = append(errs, err)
			}

			continue
		}

		if ok {
			deleted++
		}
	}

	if failed > maxDeleteErrors {
		errs = append(errs, errors.Errorf("and %v more", failed-maxDeleteErrors))
	}

	if failed > 0 {
		return deleted, errors.Wrapf(stderrors.Join(errs...), "unable to delete %v of %v files", failed, len(files))
	}

	return deleted, nil
}

// deleteRegularFile deletes a regular file, also a read-only one, and reports
// whether it did. Anything else, such as a folder or a link, is left alone.
func deleteRegularFile(path string) (bool, error) {
	fi, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}

	if err != nil {
		return false, errors.Wrapf(err, "unable to examine %v", path)
	}

	if !fi.Mode().IsRegular() {
		return false, nil
	}

	err = os.Remove(path)
	if err != nil && fi.Mode().Perm()&0o200 == 0 {
		// Windows does not delete read-only files
		if chmodErr := os.Chmod(path, 0o600); chmodErr == nil { //nolint:mnd
			err = os.Remove(path)
		}
	}

	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, errors.Wrapf(err, "unable to delete %v", path)
	}

	return true, nil
}

// DiskWithSameGUID returns the path of another attached disk that has the
// imaged disk's GUID, or "". Windows keeps one of two such disks offline, so
// the volumes restored to the target may not be mounted.
func (s *Snapshot) DiskWithSameGUID(t Target) string {
	others, err := otherDiskGUIDs(t)
	if err != nil {
		return ""
	}

	return others[s.Layout.DiskGUID]
}

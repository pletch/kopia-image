package ntfs

import (
	stderrors "errors"

	"github.com/pkg/errors"
	"golang.org/x/sys/windows"

	"github.com/kopia/kopia/snapshot/image"
)

// maxExcludeErrors bounds the errors ExcludeFiles reports.
const maxExcludeErrors = 10

// ExcludeFiles reads files as zeros and lists them in ExcludedFiles, so that
// restoring the image can delete them. root is the volume root path ending
// in a backslash, normally a shadow copy device; files are relative to it.
// folder describes where the files come from; its file count and size are
// filled in, and it is added to Excluded when any file was excluded.
//
// Files with more than one name are skipped, as their other names may be
// elsewhere, and so are reparse points, whose targets may be. Files that are
// gone are skipped too; files that cannot be examined are imaged, and the
// error returned lists them, while what was excluded stays excluded.
func (v *Volume) ExcludeFiles(root string, folder image.ExcludedFolder, files []string) error {
	var (
		errs   []error
		failed int
	)

	folder.Files, folder.Bytes = 0, 0

	for _, f := range files {
		extents, ok, err := excludableFileExtents(root+f, v.Data.ClusterSize)
		if err != nil {
			failed++

			if failed <= maxExcludeErrors {
				errs = append(errs, err)
			}

			continue
		}

		if !ok {
			continue
		}

		folder.Files++
		folder.Bytes += image.TotalLength(extents)

		v.ExcludedFiles = append(v.ExcludedFiles, f)
		v.ExcludedExtents = append(v.ExcludedExtents, extents...)
	}

	if folder.Files > 0 {
		v.Excluded = append(v.Excluded, folder)
	}

	if failed > maxExcludeErrors {
		errs = append(errs, errors.Errorf("and %v more", failed-maxExcludeErrors))
	}

	if failed > 0 {
		return errors.Wrapf(stderrors.Join(errs...), "%v: %v files are imaged", folder.Path, failed)
	}

	return nil
}

// excludableFileExtents returns the allocated extents of a file and true, or
// false when the file is gone, has other names or is a reparse point.
func excludableFileExtents(path string, clusterSize int64) ([]image.Extent, bool, error) {
	h, err := openFile(path, windows.FILE_READ_ATTRIBUTES)
	if isNotFound(err) {
		return nil, false, nil
	}

	if err != nil {
		return nil, false, err
	}

	defer windows.CloseHandle(h) //nolint:errcheck

	var info windows.ByHandleFileInformation

	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		return nil, false, errors.Wrapf(err, "unable to get information about %q", path)
	}

	if info.NumberOfLinks > 1 || info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return nil, false, nil
	}

	extents, err := handleExtents(h, path, clusterSize)

	return extents, err == nil, err
}

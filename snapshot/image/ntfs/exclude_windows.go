package ntfs

import (
	stderrors "errors"
	"io/fs"
	"os"
	"strings"

	"github.com/pkg/errors"
	"golang.org/x/sys/windows"

	"github.com/kopia/kopia/snapshot/image"
)

// Exclude reads the files below the folders dirs as zeros and lists them in
// ExcludedFiles, so that restoring the image can delete them. root is the volume
// root path ending in a backslash, normally a shadow copy device; dirs are
// relative to it. Folders that do not exist are skipped. safeAsZeros says
// whether the programs using the files recover from them holding zeros, see
// image.ExcludedFolder.
//
// Files with more than one name are skipped, as their other names may be
// outside the folders, and so are reparse points, whose targets may be: only
// regular files and directories below the folders are excluded. Files that
// cannot be opened are skipped too, so the error returned lists what could
// not be excluded, and what was excluded stays excluded.
func (v *Volume) Exclude(root string, dirs []string, safeAsZeros bool) error {
	var errs []error

	for _, dir := range dirs {
		folder, files, extents, err := excludeFolder(root, dir, v.Data.ClusterSize)
		if err != nil {
			errs = append(errs, err)
		}

		if folder.Files > 0 {
			folder.SafeAsZeros = safeAsZeros
			v.Excluded = append(v.Excluded, folder)
			v.ExcludedExtents = append(v.ExcludedExtents, extents...)
			v.ExcludedFiles = append(v.ExcludedFiles, files...)
		}
	}

	return stderrors.Join(errs...)
}

func excludeFolder(root, dir string, clusterSize int64) (image.ExcludedFolder, []string, []image.Extent, error) {
	folder := image.ExcludedFolder{Path: dir}

	var (
		files   []string
		extents []image.Extent
		errs    []error
	)

	err := walkRegularFiles(root+dir, func(path string) {
		fileExtents, ok, err := excludableFileExtents(path, clusterSize)
		if err != nil {
			errs = append(errs, err)
			return
		}

		if !ok {
			return
		}

		folder.Files++
		folder.Bytes += image.TotalLength(fileExtents)

		files = append(files, strings.TrimPrefix(path, root))
		extents = append(extents, fileExtents...)
	})
	if err != nil {
		errs = append(errs, err)
	}

	if len(errs) > 0 {
		return folder, files, extents, errors.Wrapf(stderrors.Join(errs...), "%v", dir)
	}

	return folder, files, extents, nil
}

// walkRegularFiles calls visit for each regular file below dir, without
// following reparse points. A dir that does not exist has no files.
func walkRegularFiles(dir string, visit func(path string)) error {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}

	if err != nil {
		return errors.Wrapf(err, "unable to list %q", dir)
	}

	var errs []error

	for _, e := range entries {
		path := strings.TrimSuffix(dir, `\`) + `\` + e.Name()

		// junctions, symbolic links and other reparse points are neither
		switch t := e.Type(); {
		case t == fs.ModeDir:
			if err := walkRegularFiles(path, visit); err != nil {
				errs = append(errs, err)
			}
		case t.IsRegular():
			visit(path)
		}
	}

	return stderrors.Join(errs...)
}

// excludableFileExtents returns the allocated extents of a file and true, or
// false when the file has other names or is a reparse point.
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

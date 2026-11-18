package image

import (
	"context"
	"io"
	"path"
	"slices"
	"strings"

	"github.com/pkg/errors"

	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/object"
)

const (
	excludedEntrySuffix = ".excluded"

	// maxExcludedListSize bounds the excluded file list read from an image.
	maxExcludedListSize = 1 << 30
)

// ExcludedEntryName returns the snapshot entry name for the list of files of
// a volume that were read as zeros, e.g. "p3-C.excluded".
func ExcludedEntryName(partitionNumber int, label string) string {
	return volumeEntryPrefix(partitionNumber, label) + excludedEntrySuffix
}

// EncodeExcludedFiles returns the content of an excluded file list: the
// paths relative to the volume root, separated by backslashes, one per line,
// sorted.
func EncodeExcludedFiles(files []string) []byte {
	sorted := slices.Sorted(slices.Values(files))

	var sb strings.Builder

	for _, f := range sorted {
		sb.WriteString(f)
		sb.WriteByte('\n')
	}

	return []byte(sb.String())
}

// DecodeExcludedFiles parses an excluded file list. Every path must be
// relative to the volume root and stay below it.
func DecodeExcludedFiles(data []byte) ([]string, error) {
	var files []string

	for line := range strings.SplitSeq(string(data), "\n") {
		if line == "" {
			continue
		}

		if !IsVolumeRelativePath(line) {
			return nil, errors.Errorf("invalid excluded file path %q", line)
		}

		files = append(files, line)
	}

	return files, nil
}

// IsVolumeRelativePath reports whether p, with backslash separators, names a
// file below a volume root: not absolute, no drive or stream, and no "." or
// ".." components.
func IsVolumeRelativePath(p string) bool {
	if p == "" || strings.ContainsAny(p, ":/\x00") || strings.HasPrefix(p, `\`) {
		return false
	}

	for c := range strings.SplitSeq(p, `\`) {
		if c == "" || c == "." || c == ".." {
			return false
		}
	}

	return path.Clean(strings.ReplaceAll(p, `\`, "/")) == strings.ReplaceAll(p, `\`, "/")
}

// ReadExcludedFiles returns the files of a volume that were read as zeros,
// relative to the volume root, or nil for a volume without any.
func ReadExcludedFiles(ctx context.Context, rep repo.Repository, v *Volume) ([]string, error) {
	if v.ExcludedFilesID == object.EmptyID {
		return nil, nil
	}

	r, err := rep.OpenObject(ctx, v.ExcludedFilesID)
	if err != nil {
		return nil, errors.Wrapf(err, "unable to open the excluded file list of %v", v.EntryName)
	}
	defer r.Close() //nolint:errcheck

	data, err := io.ReadAll(io.LimitReader(r, maxExcludedListSize))
	if err != nil {
		return nil, errors.Wrapf(err, "unable to read the excluded file list of %v", v.EntryName)
	}

	files, err := DecodeExcludedFiles(data)

	return files, errors.Wrapf(err, "excluded file list of %v", v.EntryName)
}

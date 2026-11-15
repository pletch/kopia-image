package image

import (
	"strings"

	"github.com/google/uuid"
	"github.com/pkg/errors"
)

const (
	// SourcePathPrefix starts the path of every image snapshot source. No
	// filesystem path stored as a snapshot source starts with it.
	SourcePathPrefix = "image:"

	diskSourcePathPrefix = SourcePathPrefix + "disk:"
)

// IsSourcePath reports whether a snapshot source path names an image source
// rather than a filesystem directory.
func IsSourcePath(path string) bool {
	return strings.HasPrefix(path, SourcePathPrefix)
}

// DiskSourcePath returns the snapshot source path for images of the disk with
// the given GPT disk GUID, e.g. "image:disk:0f6ab3f2-...".
func DiskSourcePath(diskGUID string) (string, error) {
	g, err := CanonicalGUID(diskGUID)
	if err != nil {
		return "", err
	}

	return diskSourcePathPrefix + g, nil
}

// ParseDiskSourcePath returns the disk GUID named by a disk image source path.
func ParseDiskSourcePath(path string) (string, error) {
	g, ok := strings.CutPrefix(path, diskSourcePathPrefix)
	if !ok {
		return "", errors.Errorf("%q is not a disk image source path", path)
	}

	canonical, err := CanonicalGUID(g)
	if err != nil {
		return "", err
	}

	if canonical != g {
		return "", errors.Errorf("disk GUID in %q is not in canonical form %q", path, canonical)
	}

	return canonical, nil
}

// CanonicalGUID returns a GUID in lowercase 8-4-4-4-12 form, accepting
// optional braces and any letter case.
func CanonicalGUID(s string) (string, error) {
	trimmed := strings.TrimSuffix(strings.TrimPrefix(s, "{"), "}")

	if len(trimmed) != 36 { //nolint:mnd
		return "", errors.Errorf("invalid GUID %q", s)
	}

	g, err := uuid.Parse(trimmed)
	if err != nil {
		return "", errors.Wrapf(err, "invalid GUID %q", s)
	}

	return g.String(), nil
}

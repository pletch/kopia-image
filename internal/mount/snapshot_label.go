package mount

import (
	"context"
	"runtime"
	"strings"
	"time"

	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/object"
	"github.com/kopia/kopia/snapshot"
)

const (
	snapshotLabelTimeFormat = "2006-01-02 15:04"

	// maxSnapshotLabelNameLength limits the source name part of the label, Explorer
	// truncates long drive names in most views anyway.
	maxSnapshotLabelNameLength = 40
)

// SnapshotDriveLabel returns a drive label, for use as Options.DriveLabel, that names
// the source and the time of the snapshot whose root directory is oid. When several
// snapshots share the same root, the most recent one is used. It returns an empty
// string when no snapshot has that root, for example for a subdirectory of a snapshot,
// and on platforms that do not use drive labels.
func SnapshotDriveLabel(ctx context.Context, rep repo.Repository, oid object.ID) string {
	if runtime.GOOS != "windows" {
		return ""
	}

	manifests, err := snapshot.FindSnapshotsByRootObjectID(ctx, rep, oid)
	if err != nil {
		log(ctx).Warnf("unable to find snapshot for drive label: %v", err)
		return ""
	}

	var latest *snapshot.Manifest

	for _, m := range manifests {
		if latest == nil || m.StartTime.After(latest.StartTime) {
			latest = m
		}
	}

	if latest == nil {
		return ""
	}

	return formatSnapshotDriveLabel(latest.Source.Path, latest.StartTime.ToTime().Local())
}

// formatSnapshotDriveLabel returns a label such as "Kopia - Documents - 2026-11-15 10:02"
// for a snapshot of the given source path taken at the given time.
func formatSnapshotDriveLabel(sourcePath string, startTime time.Time) string {
	label := "Kopia - "
	if name := sourceLabelName(sourcePath); name != "" {
		label += name + " - "
	}

	return label + startTime.Format(snapshotLabelTimeFormat)
}

// sourceLabelName returns the last element of a source path, which may use either
// path separator. A drive root such as "C:\" becomes "C:" and "/" is kept as is.
func sourceLabelName(sourcePath string) string {
	name := strings.TrimRight(sourcePath, `/\`)
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}

	if name == "" {
		name = sourcePath
	}

	if r := []rune(name); len(r) > maxSnapshotLabelNameLength {
		name = string(r[:maxSnapshotLabelNameLength-3]) + "..."
	}

	return name
}

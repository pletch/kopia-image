package serverapi

import (
	"time"

	"github.com/kopia/kopia/fs"
	"github.com/kopia/kopia/repo/manifest"
	"github.com/kopia/kopia/snapshot"
	"github.com/kopia/kopia/snapshot/image"
	"github.com/kopia/kopia/snapshot/image/disksource"
	"github.com/kopia/kopia/snapshot/image/schedule"
)

// ImagesResponse lists image snapshots, newest first within each source.
type ImagesResponse struct {
	Images        []*ImageSnapshot `json:"images"`
	LocalHost     string           `json:"localHost"`
	LocalUsername string           `json:"localUsername"`
}

// ImageSnapshot is a disk image snapshot with the statistics of the run
// that created it.
type ImageSnapshot struct {
	ID          manifest.ID         `json:"id"`
	Source      snapshot.SourceInfo `json:"source"`
	Description string              `json:"description"`
	StartTime   fs.UTCTimestamp     `json:"startTime"`
	EndTime     fs.UTCTimestamp     `json:"endTime"`
	Pins        []string            `json:"pins"`

	// RootID is the snapshot root, for browsing and mounting the image.
	RootID string `json:"rootID"`

	// Size is the size of the image's volumes as stored, before zeroed
	// chunks are left out.
	Size int64 `json:"size"`

	DiskGUID string        `json:"diskGuid,omitempty"`
	Volumes  []ImageVolume `json:"volumes"`

	image.Totals

	// UploadedBytes is nil when the repository did not report it.
	UploadedBytes *int64 `json:"uploadedBytes,omitempty"`

	// MetadataError says why the image's statistics and volumes are
	// missing: its image.json could not be read.
	MetadataError string `json:"metadataError,omitempty"`
}

// ImageVolume is a partition saved in an image.
type ImageVolume struct {
	EntryName       string           `json:"entryName"`
	PartitionNumber int              `json:"partitionNumber"`
	Kind            image.VolumeKind `json:"kind"`
	Size            int64            `json:"size"`
	FreeBytes       int64            `json:"freeBytes,omitempty"`
}

// ImageStatusResponse describes kopia image on the computer the server runs
// on: its scheduled tasks, its last image run and its last recovery media.
type ImageStatusResponse struct {
	// Supported is false where kopia image cannot schedule runs, i.e. on
	// computers other than Windows; the other fields are then empty.
	Supported bool `json:"supported"`

	Tasks []*ImageTask `json:"tasks"`

	// LastRun is nil when no image run has completed yet.
	LastRun      *disksource.RunStatus `json:"lastRun,omitempty"`
	LastRunError string                `json:"lastRunError,omitempty"`

	// RecoveryMedia is nil when no recovery media was built.
	RecoveryMedia      *ImageRecoveryMedia `json:"recoveryMedia,omitempty"`
	RecoveryMediaError string              `json:"recoveryMediaError,omitempty"`

	// Schedule holds the settings the image tasks were installed with; nil
	// when they are not installed, or were installed by a kopia that did not
	// record them.
	Schedule      *schedule.Settings `json:"schedule,omitempty"`
	ScheduleError string             `json:"scheduleError,omitempty"`

	// ScheduleForThisRepository tells whether the tasks write to the
	// server's repository; nil when that is not known.
	ScheduleForThisRepository *bool `json:"scheduleForThisRepository,omitempty"`

	// KopiaVersion is the server's version; when the tasks run an older copy
	// of kopia, Schedule.KopiaVersion differs.
	KopiaVersion string `json:"kopiaVersion"`
}

// ImageDisksResponse lists the disks of the server's computer that the image
// schedule can image.
type ImageDisksResponse struct {
	// Supported is false on computers other than Windows; Disks is then empty.
	Supported bool                   `json:"supported"`
	Disks     []disksource.LocalDisk `json:"disks"`
}

// ImageTask is a scheduled image task.
type ImageTask struct {
	// Name is the task's full name, e.g. `\Kopia\Image create`.
	Name string `json:"name"`

	// Scheduled is false when the task is not installed.
	Scheduled bool `json:"scheduled"`

	schedule.TaskStatus

	// Error says why the task's status could not be read.
	Error string `json:"error,omitempty"`
}

// ImageRecoveryMedia describes the last recovery media built. Whether it
// still matches the computer is in the last run's RecoveryMedia findings.
type ImageRecoveryMedia struct {
	BuiltAt          time.Time `json:"builtAt"`
	KopiaVersion     string    `json:"kopiaVersion"`
	Target           string    `json:"target"`
	PasswordEmbedded bool      `json:"passwordEmbedded,omitempty"`
}

// RunImageTaskResponse is the response of starting the scheduled image task.
type RunImageTaskResponse struct {
	// Task is the full name of the task started.
	Task string `json:"task"`
}

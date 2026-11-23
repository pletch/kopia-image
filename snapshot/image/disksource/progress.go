package disksource

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/pkg/errors"

	"github.com/kopia/kopia/snapshot/image"
)

const (
	progressFileName = "image-progress.json"
	stopFileName     = "image-stop.json"

	// ProgressInterval is how often a run updates its progress file; a file
	// not updated for much longer belongs to a run that ended without
	// removing it.
	ProgressInterval = 2 * time.Second
)

// RunProgress is what a running "kopia image create" writes to the state
// folder every ProgressInterval, for the web UI. The run removes it when it
// ends.
type RunProgress struct {
	PID       int       `json:"pid"`
	Volumes   []string  `json:"volumes"`
	StartTime time.Time `json:"startTime"`
	UpdatedAt time.Time `json:"updatedAt"`

	// Disk is the 1-based index of the disk being imaged among Disks.
	Disk       int    `json:"disk"`
	Disks      int    `json:"disks"`
	DiskNumber uint32 `json:"diskNumber"`

	image.ProgressSnapshot

	UploadedBytes int64 `json:"uploadedBytes"`

	// Stopping is set once the run saw a stop request.
	Stopping bool `json:"stopping,omitempty"`

	// Repository identifies the repository the run writes to, see
	// recoverymedia.ConnectionDigest; empty in files written before.
	Repository string `json:"repository,omitempty"`
}

// StopRequest asks the image run in progress to stop, see "kopia image stop".
type StopRequest struct {
	RequestedAt time.Time `json:"requestedAt"`
}

// ProgressPathIn returns the progress file in a state folder.
func ProgressPathIn(stateDir string) string {
	return filepath.Join(stateDir, progressFileName)
}

// StopPathIn returns the stop request file in a state folder.
func StopPathIn(stateDir string) string {
	return filepath.Join(stateDir, stopFileName)
}

// WriteRunProgress replaces the progress file.
func WriteRunProgress(path string, p *RunProgress) error {
	return errors.Wrap(writeStateJSON(path, p), "unable to write the image progress")
}

// ReadRunProgress reads the progress file. It returns an error wrapping
// os.ErrNotExist when no run is in progress.
func ReadRunProgress(path string) (*RunProgress, error) {
	var p RunProgress

	b, err := os.ReadFile(path) //nolint:gosec
	if err != nil {
		return nil, errors.Wrap(err, "unable to read the image progress")
	}

	return &p, errors.Wrap(json.Unmarshal(b, &p), "invalid image progress")
}

// WriteStopRequest asks the run in progress to stop.
func WriteStopRequest(path string, r *StopRequest) error {
	return errors.Wrap(writeStateJSON(path, r), "unable to write the stop request")
}

// ReadStopRequest reads the stop request. It returns an error wrapping
// os.ErrNotExist when there is none.
func ReadStopRequest(path string) (*StopRequest, error) {
	var r StopRequest

	b, err := os.ReadFile(path) //nolint:gosec
	if err != nil {
		return nil, errors.Wrap(err, "unable to read the stop request")
	}

	return &r, errors.Wrap(json.Unmarshal(b, &r), "invalid stop request")
}

// writeStateJSON replaces a file in the state folder atomically; the folder
// lets every user read its files.
func writeStateJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err //nolint:wrapcheck
	}

	tmp := path + ".tmp"

	if err := os.WriteFile(tmp, b, 0o644); err != nil { //nolint:gosec,mnd
		return err //nolint:wrapcheck
	}

	return os.Rename(tmp, path) //nolint:wrapcheck
}

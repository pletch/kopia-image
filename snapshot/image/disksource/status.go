package disksource

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/pkg/errors"
)

const statusFileName = "image-last-run.json"

// RunStatus records the outcome of the last "kopia image create" run, for
// "kopia image schedule show" and for monitoring tools.
type RunStatus struct {
	Volumes   []string  `json:"volumes"`
	StartTime time.Time `json:"startTime"`
	EndTime   time.Time `json:"endTime"`
	Success   bool      `json:"success"`
	Error     string    `json:"error,omitempty"`

	// Images are the image snapshots the run saved, also when it failed later.
	Images []RunImage `json:"images,omitempty"`

	// RecoveryMedia lists why the recovery media last built no longer
	// matches the computer, after a successful run.
	RecoveryMedia []string `json:"recoveryMedia,omitempty"`
}

// RunImage is an image snapshot saved by a run.
type RunImage struct {
	Source     string `json:"source"`
	SnapshotID string `json:"snapshotId"`
	BytesRead  int64  `json:"bytesRead"`

	// UploadedBytes is nil when the repository does not report it.
	UploadedBytes *int64 `json:"uploadedBytes,omitempty"`
}

// StatusPath returns the status file path for a shadow state file path; the
// two live in the same folder.
func StatusPath(statePath string) string {
	return StatusPathIn(filepath.Dir(statePath))
}

// StatusPathIn returns the status file path in a state folder.
func StatusPathIn(stateDir string) string {
	return filepath.Join(stateDir, statusFileName)
}

// WriteRunStatus replaces the status file atomically.
func WriteRunStatus(path string, s *RunStatus) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return errors.Wrap(err, "unable to encode the run status")
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil { //nolint:mnd
		return errors.Wrap(err, "unable to create the status folder")
	}

	tmp := path + ".tmp"

	if err := os.WriteFile(tmp, b, 0o644); err != nil { //nolint:gosec,mnd
		return errors.Wrap(err, "unable to write the run status")
	}

	return errors.Wrap(os.Rename(tmp, path), "unable to save the run status")
}

// ReadRunStatus reads the status file. It returns an error wrapping
// os.ErrNotExist if no run has recorded one.
func ReadRunStatus(path string) (*RunStatus, error) {
	b, err := os.ReadFile(path) //nolint:gosec
	if err != nil {
		return nil, errors.Wrap(err, "unable to read the run status")
	}

	var s RunStatus

	return &s, errors.Wrap(json.Unmarshal(b, &s), "invalid run status")
}

package schedule

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/pkg/errors"
)

const (
	settingsFileName = "image-schedule.json"
	requestFileName  = "image-schedule-request.json"
)

// Settings are the settings "kopia image schedule install" scheduled the
// image tasks with. They are kept in StateDir, readable by every user, so
// that the web UI can show and change them; Task Scheduler does not let
// other users read the tasks of SYSTEM.
type Settings struct {
	Volumes []string `json:"volumes"`

	// At is the time of day, HH:MM; Days are weekday names, empty for
	// every day.
	At   string   `json:"at"`
	Days []string `json:"days,omitempty"`

	TimeLimit      string `json:"timeLimit"`
	Retries        int    `json:"retries"`
	RetryInterval  string `json:"retryInterval"`
	Wake           bool   `json:"wake"`
	AllowOnBattery bool   `json:"allowOnBattery"`

	// CheckDay is the weekday of the weekly image check, or "none".
	CheckDay    string  `json:"checkDay"`
	CheckAt     string  `json:"checkAt"`
	CheckSample float64 `json:"checkSample"`
	CheckNTFS   bool    `json:"checkNtfs"`

	// Paused is set while "kopia image schedule pause" keeps the tasks from
	// starting.
	Paused bool `json:"paused,omitempty"`

	// KopiaVersion is the version of the copy of kopia the tasks run.
	KopiaVersion string    `json:"kopiaVersion"`
	InstalledAt  time.Time `json:"installedAt"`

	// Repository describes the repository the tasks write to, and
	// ConnectionDigest identifies it, see recoverymedia.ConnectionDigest.
	Repository       string `json:"repository"`
	ConnectionDigest string `json:"connectionDigest"`
}

// Request is the outcome of the last "kopia image schedule install" or
// "remove" run with a request ID. KopiaUI runs those as administrator
// without a window, so it reads why they failed from here.
type Request struct {
	ID        string    `json:"id"`
	Operation string    `json:"operation"`
	Time      time.Time `json:"time"`
	Success   bool      `json:"success"`
	Error     string    `json:"error,omitempty"`
}

// SettingsPath returns the file in stateDir that holds the schedule
// settings.
func SettingsPath(stateDir string) string {
	return filepath.Join(stateDir, settingsFileName)
}

// RequestPath returns the file in stateDir that holds the last request.
func RequestPath(stateDir string) string {
	return filepath.Join(stateDir, requestFileName)
}

// ReadSettings reads the schedule settings. It returns an error wrapping
// os.ErrNotExist when images are not scheduled, or were scheduled by a kopia
// that did not record them.
func ReadSettings(path string) (*Settings, error) {
	var s Settings
	if err := readJSON(path, &s); err != nil {
		return nil, errors.Wrap(err, "unable to read the image schedule settings")
	}

	return &s, nil
}

// ReadRequest reads the outcome of the last request.
func ReadRequest(path string) (*Request, error) {
	var r Request
	if err := readJSON(path, &r); err != nil {
		return nil, errors.Wrap(err, "unable to read the image schedule request")
	}

	return &r, nil
}

// WriteSettings replaces the schedule settings.
func WriteSettings(path string, s *Settings) error {
	return errors.Wrap(writeJSON(path, s), "unable to save the image schedule settings")
}

// WriteRequest replaces the outcome of the last request.
func WriteRequest(path string, r *Request) error {
	return errors.Wrap(writeJSON(path, r), "unable to save the image schedule request")
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path) //nolint:gosec
	if err != nil {
		return err //nolint:wrapcheck
	}

	return errors.Wrapf(json.Unmarshal(b, v), "invalid %v", path)
}

// writeJSON replaces a file atomically.
func writeJSON(path string, v any) error {
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

package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// imageScheduleTimeout bounds "kopia image schedule install" and "remove"
// once the administrator prompt was accepted: install copies kopia and opens
// the repository.
const imageScheduleTimeout = 5 * time.Minute

// imageScheduleRequestFile is where kopia records the outcome of a schedule
// install or remove run with --request-id, in ProgramData\kopia; it matches
// schedule.RequestPath in the kopia module.
const imageScheduleRequestFile = "image-schedule-request.json"

var (
	errInvalidSchedule = errors.New("invalid image schedule")

	driveLetterRE = regexp.MustCompile(`^[A-Z]:$`)
	timeOfDayRE   = regexp.MustCompile(`^([01]\d|2[0-3]):[0-5]\d$`)

	weekdays = []string{"Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"}
)

// imageScheduleSettings are the schedule settings the web UI sends. They end
// up on the command line of a program run as administrator, so every value
// is checked against a fixed form before use.
type imageScheduleSettings struct {
	// Volumes are drive letters, one on each disk to image, e.g. "C:".
	Volumes []string `json:"volumes"`

	// At is the time of day, HH:MM; Days are weekday names, none for every
	// day.
	At   string   `json:"at"`
	Days []string `json:"days"`

	// CheckDay is the weekday of the weekly image check, or "none".
	CheckDay string `json:"checkDay"`
	CheckAt  string `json:"checkAt"`

	Wake           bool `json:"wake"`
	AllowOnBattery bool `json:"allowOnBattery"`

	// Disks are schedules of their own for some disks, each run by its
	// own task; the disks of Volumes are imaged at At on Days.
	Disks []imageDiskSchedule `json:"disks"`
}

// imageDiskSchedule is when the disks holding Volumes are imaged.
type imageDiskSchedule struct {
	Volumes []string `json:"volumes"`
	At      string   `json:"at"`
	Days    []string `json:"days"`
}

// maxImageDiskSchedules matches schedule.MaxDiskSchedules in the kopia
// module: one for each drive letter.
const maxImageDiskSchedules = 26

// installArgs checks the settings and returns the arguments of
// "kopia image schedule install" for the repository connection configFile.
func (s *imageScheduleSettings) installArgs(configFile, requestID string) ([]string, error) {
	if len(s.Disks) > maxImageDiskSchedules {
		return nil, fmt.Errorf("%w: too many schedules", errInvalidSchedule)
	}

	// every volume once, in all the schedules together
	all := slices.Clone(s.Volumes)
	for _, d := range s.Disks {
		if len(d.Volumes) == 0 {
			return nil, fmt.Errorf("%w: a schedule has no disk", errInvalidSchedule)
		}

		all = append(all, d.Volumes...)
	}

	if err := validateVolumes(all); err != nil {
		return nil, err
	}

	if err := validateWhen(s.At, s.Days); err != nil {
		return nil, err
	}

	for _, d := range s.Disks {
		if err := validateWhen(d.At, d.Days); err != nil {
			return nil, err
		}
	}

	if !timeOfDayRE.MatchString(s.CheckAt) {
		return nil, fmt.Errorf("%w: %q is not a time of day (HH:MM)", errInvalidSchedule, s.CheckAt)
	}

	if s.CheckDay != "none" && !slices.Contains(weekdays, s.CheckDay) {
		return nil, fmt.Errorf("%w: %q is not a weekday", errInvalidSchedule, s.CheckDay)
	}

	args := []string{"--config-file", configFile, "image", "schedule", "install"}
	args = append(args, s.Volumes...)
	args = append(args, "--at", s.At, "--check-day", s.CheckDay, "--check-at", s.CheckAt)

	if len(s.Days) > 0 {
		args = append(args, "--days", strings.Join(s.Days, ","))
	}

	for _, d := range s.Disks {
		spec := strings.Join(d.Volumes, ",") + "=" + d.At
		if len(d.Days) > 0 {
			spec += "/" + strings.Join(d.Days, ",")
		}

		args = append(args, "--disk-schedule", spec)
	}

	args = append(args, boolFlag("wake", s.Wake), boolFlag("allow-on-battery", s.AllowOnBattery), "--request-id", requestID)

	return args, nil
}

// validateWhen accepts a time of day, HH:MM, and distinct weekday names.
func validateWhen(at string, days []string) error {
	if !timeOfDayRE.MatchString(at) {
		return fmt.Errorf("%w: %q is not a time of day (HH:MM)", errInvalidSchedule, at)
	}

	for i, d := range days {
		if !slices.Contains(weekdays, d) || slices.Contains(days[:i], d) {
			return fmt.Errorf("%w: %q is not a weekday", errInvalidSchedule, d)
		}
	}

	return nil
}

// validateVolumes accepts one to 26 distinct drive letters, e.g. "C:": they
// go on a command line run as administrator.
func validateVolumes(volumes []string) error {
	if len(volumes) == 0 || len(volumes) > 26 {
		return fmt.Errorf("%w: choose at least one disk", errInvalidSchedule)
	}

	for i, v := range volumes {
		if !driveLetterRE.MatchString(v) || slices.Contains(volumes[:i], v) {
			return fmt.Errorf("%w: %q is not a drive letter", errInvalidSchedule, v)
		}
	}

	return nil
}

func boolFlag(name string, v bool) string {
	if v {
		return "--" + name
	}

	return "--no-" + name
}

// imageScheduleOutcome is what kopia records for a request, see
// schedule.Request in the kopia module.
type imageScheduleOutcome struct {
	ID      string `json:"id"`
	Success bool   `json:"success"`
	Error   string `json:"error"`
}

// installImageSchedule schedules images of this computer with the
// repository of a window, as administrator after the UAC prompt.
func (s *shell) installImageSchedule(repoID string, settings *imageScheduleSettings) error {
	if s.server(repoID) == nil {
		return fmt.Errorf("unknown repository %q", repoID)
	}

	id, err := newRequestID()
	if err != nil {
		return err
	}

	args, err := settings.installArgs(s.configs.configFile(repoID), id)
	if err != nil {
		return err
	}

	return s.runImageScheduleRequest("install", id, args)
}

// removeImageSchedule removes the scheduled image tasks and their copy of
// the repository connection, as administrator after the UAC prompt.
func (s *shell) removeImageSchedule() error {
	id, err := newRequestID()
	if err != nil {
		return err
	}

	return s.runImageScheduleRequest("remove", id, []string{"image", "schedule", "remove", "--request-id", id})
}

// updateImageSchedule makes the scheduled image tasks run the kopia that
// KopiaUI bundles, as administrator after the UAC prompt. The tasks keep their
// settings and their own repository connection.
func (s *shell) updateImageSchedule() error {
	id, err := newRequestID()
	if err != nil {
		return err
	}

	return s.runImageScheduleRequest("update", id, []string{"image", "schedule", "update", "--request-id", id})
}

// pauseImageSchedule keeps the scheduled image tasks from starting, or lets
// them start again when resume is set, as administrator after the UAC
// prompt.
func (s *shell) pauseImageSchedule(resume bool) error {
	id, err := newRequestID()
	if err != nil {
		return err
	}

	operation := "pause"
	if resume {
		operation = "resume"
	}

	return s.runImageScheduleRequest(operation, id, []string{"image", "schedule", operation, "--request-id", id})
}

// runImageScheduleRequest runs kopia as administrator without a window and
// reports why it failed from the outcome kopia records under the request ID.
func (s *shell) runImageScheduleRequest(operation, id string, args []string) error {
	if !s.elevationMu.TryLock() {
		return errElevationBusy
	}
	defer s.elevationMu.Unlock()

	run := s.runElevated
	if run == nil {
		run = runElevatedHidden
	}

	code, err := run(s.kopiaPath, args, imageScheduleTimeout)
	if err != nil {
		return err
	}

	outcome, readErr := s.readImageScheduleOutcome(id)

	switch {
	case readErr == nil && !outcome.Success:
		return fmt.Errorf("kopia image schedule %v failed: %v", operation, outcome.Error)
	case readErr == nil || code == 0:
		slog.Info("image schedule changed as administrator", "operation", operation)
		return nil
	default:
		slog.Warn("kopia image schedule failed", "operation", operation, "exitCode", code, "err", readErr)
		return fmt.Errorf("kopia image schedule %v failed with exit code %v", operation, code)
	}
}

// readImageScheduleOutcome reads the outcome kopia recorded for a request.
func (s *shell) readImageScheduleOutcome(id string) (*imageScheduleOutcome, error) {
	dir := s.imageStateDir
	if dir == "" {
		d, err := kopiaStateDir()
		if err != nil {
			return nil, err
		}

		dir = d
	}

	b, err := os.ReadFile(filepath.Join(dir, imageScheduleRequestFile)) //nolint:gosec
	if err != nil {
		return nil, err //nolint:wrapcheck
	}

	var o imageScheduleOutcome
	if err := json.Unmarshal(b, &o); err != nil {
		return nil, err //nolint:wrapcheck
	}

	if o.ID != id {
		return nil, errors.New("kopia did not record the outcome")
	}

	return &o, nil
}

func newRequestID() (string, error) {
	b := make([]byte, 16) //nolint:mnd

	if _, err := rand.Read(b); err != nil {
		return "", err //nolint:wrapcheck
	}

	return hex.EncodeToString(b), nil
}

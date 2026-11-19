package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// errElevationCanceled is returned when the person declines the
// administrator prompt.
var errElevationCanceled = errors.New("the administrator prompt was declined")

// consoleRunner opens a console window that runs a program as an
// administrator, after the UAC prompt; tests replace it.
type consoleRunner func(exe string, args []string) error

func (s *shell) elevatedConsole() consoleRunner {
	if s.runConsole != nil {
		return s.runConsole
	}

	return runElevatedConsole
}

// maxDriverHosts bounds the computers whose drivers media is built with.
const maxDriverHosts = 64

// driverHostRE matches a computer name as kopia records it. It cannot start
// with "-", so that it is never taken for a flag.
var driverHostRE = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,252}$`)

// errInvalidMedia is returned for recovery media options the UI must not send.
var errInvalidMedia = fmt.Errorf("%w: invalid recovery media options", errInvalidSchedule)

// validateDriverHosts checks the computers whose drivers the media gets.
func validateDriverHosts(hosts []string) error {
	if len(hosts) > maxDriverHosts {
		return fmt.Errorf("%w: too many computers", errInvalidMedia)
	}

	for i, h := range hosts {
		if !driverHostRE.MatchString(h) || strings.EqualFold(h, "all") ||
			slices.ContainsFunc(hosts[:i], func(o string) bool { return strings.EqualFold(o, h) }) {
			return fmt.Errorf("%w: %q is not a computer name", errInvalidMedia, h)
		}
	}

	return nil
}

// recoveryMediaRequest is what the web UI asks recovery media to be built
// with.
type recoveryMediaRequest struct {
	// DriverHosts are the computers whose drivers the media gets.
	DriverHosts []string `json:"driverHosts"`

	// AllDrivers adds the drivers of every computer whose images store
	// them, including computers imaged later than the page was loaded.
	AllDrivers bool `json:"allDrivers"`

	// ISO saves the media as an ISO file, which a save dialog names, rather
	// than erasing a USB disk.
	ISO bool `json:"iso"`
}

// startRecoveryMedia opens an administrator console that runs
// "kopia image recovery-media create" for a repository, adding the drivers
// stored with the images of the requested computers. Unless an ISO file is
// asked for, the command lists the USB disks, asks which one to erase and
// shows its progress; building the media needs an administrator, which
// KopiaUI does not run as. It returns errElevationCanceled when the person
// declines the prompt or cancels the save dialog.
func (s *shell) startRecoveryMedia(repoID string, req *recoveryMediaRequest) error {
	if s.server(repoID) == nil {
		return fmt.Errorf("unknown repository %q", repoID)
	}

	if err := validateDriverHosts(req.DriverHosts); err != nil {
		return err
	}

	args := []string{
		"--config-file", s.configs.configFile(repoID),
		"image", "recovery-media", "create",
	}

	if req.AllDrivers {
		args = append(args, "--drivers-from", "all")
	} else {
		for _, h := range req.DriverHosts {
			args = append(args, "--drivers-from", h)
		}
	}

	if req.ISO {
		path, err := s.isoSaveDialog()(defaultISOName())
		if err != nil {
			return err
		}

		if path == "" {
			return errElevationCanceled
		}

		if err := validateISOPath(path); err != nil {
			return err
		}

		args = append(args, "--iso", path)
	}

	return s.elevatedConsole()(s.kopiaPath, args)
}

// isoSaveDialog asks where to save an ISO file, suggesting a name, and
// returns "" when the person cancels.
type isoSaveDialog func(name string) (string, error)

func (s *shell) isoSaveDialog() isoSaveDialog {
	if s.saveISO != nil {
		return s.saveISO
	}

	return func(name string) (string, error) {
		d := s.app.Dialog.SaveFile().
			SetMessage("Save the recovery media as").
			SetFilename(name).
			AddFilter("ISO files (*.iso)", "*.iso").
			CanCreateDirectories(true)

		if home, err := os.UserHomeDir(); err == nil {
			d.SetDirectory(filepath.Join(home, "Documents"))
		}

		if w := s.app.Window.Current(); w != nil {
			d.AttachToWindow(w)
		}

		path, err := d.PromptForSingleSelection()
		if err != nil {
			return "", fmt.Errorf("unable to ask where to save the ISO file: %w", err)
		}

		if path != "" && !strings.EqualFold(filepath.Ext(path), ".iso") {
			path += ".iso"
		}

		return path, nil
	}
}

func defaultISOName() string {
	computer, _ := os.Hostname()

	return "kopia-recovery-" + computer + "-" + time.Now().Format("2006-01-02") + ".iso"
}

// validateISOPath checks the ISO file's path, which reaches the
// administrator command line: an absolute .iso path that does not exist.
func validateISOPath(path string) error {
	if !filepath.IsAbs(path) || filepath.VolumeName(path) == "" || !strings.EqualFold(filepath.Ext(path), ".iso") ||
		strings.HasPrefix(path, "-") || strings.ContainsAny(path, "\"\x00\r\n") {
		return fmt.Errorf("%w: %q is not a path for an ISO file", errInvalidMedia, path)
	}

	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("%w: %v already exists; choose another name", errInvalidMedia, path)
	}

	return nil
}

// startImage starts "kopia image create" as administrator, after the UAC
// prompt, without a window, for a repository, imaging the disks that hold
// the given volumes, so that images can be created without a schedule. It
// does not wait: the run reports its progress, and a failure to start,
// through the state folder, which the Images page shows.
func (s *shell) startImage(repoID string, volumes []string) error {
	if s.server(repoID) == nil {
		return fmt.Errorf("unknown repository %q", repoID)
	}

	if err := validateVolumes(volumes); err != nil {
		return err
	}

	id, err := newRequestID()
	if err != nil {
		return err
	}

	args := []string{"--config-file", s.configs.configFile(repoID), "image", "create"}
	args = append(args, volumes...)
	args = append(args, "--request-id", id)

	run := s.runDetached
	if run == nil {
		run = runElevatedDetached
	}

	return run(s.kopiaPath, args)
}

// stopImage asks the image run in progress to stop, as administrator after
// the UAC prompt; the run keeps what it uploaded.
func (s *shell) stopImage() error {
	id, err := newRequestID()
	if err != nil {
		return err
	}

	return s.runImageScheduleRequest("stop", id, []string{"image", "stop", "--request-id", id})
}

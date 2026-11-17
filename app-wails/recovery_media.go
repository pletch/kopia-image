package main

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
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

// startRecoveryMedia opens an administrator console that runs
// "kopia image recovery-media create" for a repository, adding the drivers
// stored with the images of driverHosts. The command lists the USB disks,
// asks which one to erase and shows its progress; building the media needs
// an administrator, which KopiaUI does not run as. It returns
// errElevationCanceled when the person declines the prompt.
func (s *shell) startRecoveryMedia(repoID string, driverHosts []string) error {
	if s.server(repoID) == nil {
		return fmt.Errorf("unknown repository %q", repoID)
	}

	if err := validateDriverHosts(driverHosts); err != nil {
		return err
	}

	args := []string{
		"--config-file", s.configs.configFile(repoID),
		"image", "recovery-media", "create",
	}

	for _, h := range driverHosts {
		args = append(args, "--drivers-from", h)
	}

	return s.elevatedConsole()(s.kopiaPath, args)
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

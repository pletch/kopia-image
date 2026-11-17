package main

import (
	"errors"
	"fmt"
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

// startRecoveryMedia opens an administrator console that runs
// "kopia image recovery-media create" for a repository. The command lists
// the USB disks, asks which one to erase and shows its progress; building
// the media needs an administrator, which KopiaUI does not run as. It
// returns errElevationCanceled when the person declines the prompt.
func (s *shell) startRecoveryMedia(repoID string) error {
	if s.server(repoID) == nil {
		return fmt.Errorf("unknown repository %q", repoID)
	}

	return s.elevatedConsole()(s.kopiaPath, []string{
		"--config-file", s.configs.configFile(repoID),
		"image", "recovery-media", "create",
	})
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

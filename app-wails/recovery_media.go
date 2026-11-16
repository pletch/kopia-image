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

// startImage opens an administrator console that runs "kopia image create"
// for a repository, imaging the disks that hold the given volumes, so that
// images can be created without a schedule. The console shows its progress;
// closing it cancels the run.
func (s *shell) startImage(repoID string, volumes []string) error {
	if s.server(repoID) == nil {
		return fmt.Errorf("unknown repository %q", repoID)
	}

	if err := validateVolumes(volumes); err != nil {
		return err
	}

	args := []string{"--config-file", s.configs.configFile(repoID), "image", "create"}

	return s.elevatedConsole()(s.kopiaPath, append(args, volumes...))
}

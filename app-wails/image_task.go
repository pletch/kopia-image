package main

import (
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// imageTaskTimeout bounds how long "kopia image schedule run" may take once
// the administrator prompt was accepted; it only starts the task.
const imageTaskTimeout = 2 * time.Minute

// errElevationBusy is returned while an administrator prompt from KopiaUI
// is already open.
var errElevationBusy = errors.New("an administrator prompt is already open")

// elevatedRunner runs a program as an administrator after the UAC prompt
// and returns its exit code; tests replace it.
type elevatedRunner func(exe string, args []string, timeout time.Duration) (uint32, error)

// runImageTaskElevated starts the scheduled image task with
// "kopia image schedule run" as an administrator, after the UAC prompt. The
// web UI asks for it when the repository server, which runs as the person,
// may not start the task, which runs as SYSTEM. The command is fixed: the web
// UI cannot choose what runs as administrator.
func (s *shell) runImageTaskElevated() error {
	if !s.elevationMu.TryLock() {
		return errElevationBusy
	}
	defer s.elevationMu.Unlock()

	run := s.runElevated
	if run == nil {
		run = runElevatedHidden
	}

	code, err := run(s.kopiaPath, []string{"image", "schedule", "run"}, imageTaskTimeout)
	if err != nil {
		return err
	}

	if code != 0 {
		slog.Warn("kopia image schedule run failed", "exitCode", code)
		return fmt.Errorf("kopia image schedule run failed with exit code %v; 'kopia image schedule show' tells more", code)
	}

	slog.Info("started the scheduled image task as administrator")

	return nil
}

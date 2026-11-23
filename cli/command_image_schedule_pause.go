package cli

import (
	"context"
	"os"

	"github.com/pkg/errors"

	"github.com/kopia/kopia/snapshot/image/schedule"
)

// commandImageSchedulePause disables or enables the scheduled image tasks,
// keeping their settings. A run in progress goes on; a paused task neither
// starts on its schedule nor retries a failed run.
type commandImageSchedulePause struct {
	resume bool

	loc imageScheduleLocation
	req imageScheduleRequest
	out textOutput
}

func (c *commandImageSchedulePause) setup(svc appServices, parent commandParent, resume bool) {
	c.resume = resume

	name, help, operation := "pause", "Keep the scheduled image tasks from starting until resumed. Run as administrator.", "pause"
	if resume {
		name, help, operation = "resume", "Let paused image tasks start on their schedule again. Run as administrator.", "resume"
	}

	cmd := parent.Command(name, help)
	c.loc.flags(cmd)
	c.req.flag(cmd)
	cmd.Action(svc.noRepositoryAction(c.req.recording(operation, &c.loc, c.run)))
	c.out.setup(svc)
}

func (c *commandImageSchedulePause) run(ctx context.Context) error {
	if err := c.loc.resolve(); err != nil {
		return err
	}

	changed := 0

	for _, name := range c.loc.scheduledTaskNames() {
		err := schedule.SetEnabled(ctx, c.loc.taskName(name), c.resume)

		switch {
		// the weekly check may be turned off
		case errors.Is(err, schedule.ErrTaskNotFound):
		case err != nil:
			return err //nolint:wrapcheck
		default:
			changed++
		}
	}

	if changed == 0 {
		return errors.New("images are not scheduled on this computer")
	}

	if err := c.recordPaused(); err != nil {
		return err
	}

	if c.resume {
		c.out.printStdout("The scheduled image tasks start on their schedule again.\n")
	} else {
		c.out.printStdout("The scheduled image tasks are paused; a run in progress goes on.\n")
	}

	return nil
}

// recordPaused records the state in the schedule settings, which the web UI
// reads: Task Scheduler does not show the tasks of SYSTEM to other users.
func (c *commandImageSchedulePause) recordPaused() error {
	path := schedule.SettingsPath(c.loc.stateDir())

	s, err := schedule.ReadSettings(path)
	if errors.Is(err, os.ErrNotExist) {
		// scheduled by a kopia that did not record settings
		return nil
	}

	if err != nil {
		return err //nolint:wrapcheck
	}

	s.Paused = !c.resume

	return schedule.WriteSettings(path, s) //nolint:wrapcheck
}

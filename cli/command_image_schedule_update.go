package cli

import (
	"context"
	"os"

	"github.com/pkg/errors"

	"github.com/kopia/kopia/internal/clock"
	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/snapshot/image/schedule"
)

// Lines "kopia image schedule update --check" prints, for the KopiaUI-Wails
// installer.
const (
	imageScheduleNotScheduled = "not-scheduled"
	imageScheduleUpToDate     = "up-to-date"
	imageScheduleOutdated     = "outdated"
)

// commandImageScheduleUpdate replaces the scheduled tasks' copy of kopia with
// this one after an upgrade. The tasks keep their settings, repository
// connection and password, so unlike install it needs neither the
// repository connection nor its password.
type commandImageScheduleUpdate struct {
	check    bool
	kopiaExe string

	loc imageScheduleLocation
	req imageScheduleRequest
	out textOutput
}

func (c *commandImageScheduleUpdate) setup(svc appServices, parent commandParent) {
	cmd := parent.Command("update", "Make the scheduled image tasks run this version of kopia. Run as administrator.")
	cmd.Flag("check", "Only print whether the tasks run another version: not-scheduled, up-to-date, or outdated followed by both versions; needs no administrator").BoolVar(&c.check)
	cmd.Flag("kopia-exe", "kopia executable copied for the tasks to run (default: this one)").Hidden().StringVar(&c.kopiaExe)
	c.loc.flags(cmd)
	c.req.flag(cmd)
	cmd.Action(svc.noRepositoryAction(c.req.recording("update", &c.loc, c.run)))
	c.out.setup(svc)
}

func (c *commandImageScheduleUpdate) run(_ context.Context) error {
	if err := c.loc.resolve(); err != nil {
		return err
	}

	settingsPath := schedule.SettingsPath(c.loc.stateDir())

	s, err := schedule.ReadSettings(settingsPath)
	if errors.Is(err, os.ErrNotExist) {
		if c.check {
			c.out.printStdout("%v\n", imageScheduleNotScheduled)
			return nil
		}

		return errors.New("images are not scheduled on this computer, or were scheduled by a kopia that did not record the settings; run 'kopia image schedule install'")
	}

	if err != nil {
		return err //nolint:wrapcheck
	}

	if c.check {
		if s.KopiaVersion == repo.BuildVersion {
			c.out.printStdout("%v %v\n", imageScheduleUpToDate, repo.BuildVersion)
		} else {
			c.out.printStdout("%v %v %v\n", imageScheduleOutdated, s.KopiaVersion, repo.BuildVersion)
		}

		return nil
	}

	// the tasks' connection and password stay as they are
	if _, err := os.Stat(c.loc.configFile()); err != nil {
		return errors.Wrap(err, "the scheduled tasks' repository connection is missing; run 'kopia image schedule install'")
	}

	if err := installTaskExecutable(&c.loc, c.kopiaExe); err != nil {
		return err
	}

	s.KopiaVersion = repo.BuildVersion
	s.InstalledAt = clock.Now()

	if err := schedule.WriteSettings(settingsPath, s); err != nil {
		return err //nolint:wrapcheck
	}

	c.out.printStdout("The scheduled image tasks now run kopia %v.\n", repo.BuildVersion)

	return nil
}

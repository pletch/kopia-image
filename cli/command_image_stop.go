package cli

import (
	"context"
	"os"

	"github.com/pkg/errors"

	"github.com/kopia/kopia/internal/clock"
	"github.com/kopia/kopia/snapshot/image/disksource"
)

// staleProgress is how old a progress file may be before its run counts as
// ended: a run updates it every disksource.ProgressInterval.
const staleProgress = 30 * disksource.ProgressInterval

// commandImageStop asks the image run in progress to stop. The run notices
// within seconds, keeps what it uploaded, removes its shadow copies and ends
// without counting as failed, so that Task Scheduler does not retry it.
type commandImageStop struct {
	loc imageScheduleLocation
	req imageScheduleRequest
	out textOutput
}

func (c *commandImageStop) setup(svc appServices, parent commandParent) {
	cmd := parent.Command("stop", "Stop the image run in progress, keeping what it uploaded. Run as administrator.")
	cmd.Flag("state-file", "Shadow copy state file of the run").Hidden().StringVar(&c.loc.statePath)
	c.req.flag(cmd)
	cmd.Action(svc.noRepositoryAction(c.req.recording("stop", &c.loc, c.run)))
	c.out.setup(svc)
}

func (c *commandImageStop) run(ctx context.Context) error {
	if c.loc.statePath == "" {
		p, err := disksource.DefaultStatePath()
		if err != nil {
			return err //nolint:wrapcheck
		}

		c.loc.statePath = p
	}

	dir := c.loc.stateDir()

	p, err := disksource.ReadRunProgress(disksource.ProgressPathIn(dir))
	if errors.Is(err, os.ErrNotExist) || (err == nil && clock.Now().Sub(p.UpdatedAt) > staleProgress) {
		return errors.New("no image is being created")
	}

	if err != nil {
		return err //nolint:wrapcheck
	}

	if err := disksource.WriteStopRequest(disksource.StopPathIn(dir), &disksource.StopRequest{RequestedAt: clock.Now()}); err != nil {
		return err //nolint:wrapcheck
	}

	log(ctx).Infof("Asked the image run to stop; it keeps what it uploaded and removes its shadow copy.")

	return nil
}

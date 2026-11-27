package cli

import (
	"context"

	"github.com/pkg/errors"

	"github.com/kopia/kopia/repo"
)

type commandRepositoryThrottleSet struct {
	cts commonThrottleSet

	configFile func() string
}

func (c *commandRepositoryThrottleSet) setup(svc appServices, parent commandParent) {
	cmd := parent.Command("set", "Set throttling parameters for a repository")
	c.cts.setup(cmd)

	c.configFile = svc.repositoryConfigFileName

	cmd.Action(svc.repositoryReaderAction(c.run))
}

func (c *commandRepositoryThrottleSet) run(ctx context.Context, rep repo.Repository) error {
	dr, direct := rep.(repo.DirectRepository)

	limits, err := repo.ConnectionThrottling(c.configFile())
	if err != nil {
		return errors.Wrap(err, "unable to read throttling limits")
	}

	if direct {
		limits = dr.Throttler().Limits()
	}

	var changeCount int

	if err := c.cts.apply(ctx, &limits, &changeCount); err != nil {
		return err
	}

	if changeCount == 0 {
		log(ctx).Info("No changes made.")
		return nil
	}

	if direct {
		return errors.Wrap(dr.Throttler().SetLimits(limits), "error setting limits")
	}

	// a repository server connection throttles its network connection: only
	// the upload and download speeds apply, from its next connection on.
	if err := repo.SetConnectionThrottling(c.configFile(), limits); err != nil {
		return errors.Wrap(err, "error setting limits")
	}

	log(ctx).Info("Saved. Upload and download speed limits apply to this repository server connection from its next connection; the other limits apply to direct connections only.")

	return nil
}

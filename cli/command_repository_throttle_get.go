package cli

import (
	"context"

	"github.com/pkg/errors"

	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/blob/throttling"
)

type commandRepositoryThrottleGet struct {
	ctg commonThrottleGet

	configFile func() string
}

func (c *commandRepositoryThrottleGet) setup(svc appServices, parent commandParent) {
	cmd := parent.Command("get", "Get throttling parameters for a repository")
	c.ctg.setup(svc, cmd)

	c.configFile = svc.repositoryConfigFileName

	cmd.Action(svc.repositoryReaderAction(c.run))
}

func (c *commandRepositoryThrottleGet) run(_ context.Context, rep repo.Repository) error {
	var limits throttling.Limits

	if dr, ok := rep.(repo.DirectRepository); ok {
		limits = dr.Throttler().Limits()
	} else {
		// a repository server connection keeps its limits in its configuration.
		l, err := repo.ConnectionThrottling(c.configFile())
		if err != nil {
			return errors.Wrap(err, "unable to read throttling limits")
		}

		limits = l
	}

	if err := c.ctg.output(&limits); err != nil {
		return errors.Wrap(err, "output")
	}

	return nil
}

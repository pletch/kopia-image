package cli

import (
	"context"

	"github.com/pkg/errors"

	"github.com/kopia/kopia/internal/apiclient"
	"github.com/kopia/kopia/internal/serverapi"
)

type commandServerUnmount struct {
	sf serverClientFlags

	path string

	out textOutput
}

func (c *commandServerUnmount) setup(svc appServices, parent commandParent) {
	cmd := parent.Command("unmount", "Unmount a snapshot or image the server mounted, such as with 'Mount & Browse'")
	cmd.Arg("path", "Where it is mounted, such as a drive letter (X:)").Required().StringVar(&c.path)
	c.sf.setup(svc, cmd)
	c.out.setup(svc)
	cmd.Action(svc.serverAction(&c.sf, c.run))
}

func (c *commandServerUnmount) run(ctx context.Context, cli *apiclient.KopiaAPIClient) error {
	var res serverapi.MountedSnapshot

	if err := cli.Post(ctx, "control/unmount", &serverapi.UnmountPathRequest{Path: c.path}, &res); err != nil {
		return errors.Wrapf(err, "unable to unmount %v", c.path)
	}

	c.out.printStdout("Unmounted %v\n", res.Path)

	return nil
}

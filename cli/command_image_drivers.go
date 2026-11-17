package cli

import (
	"context"
	"slices"

	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/snapshot/image"
	"github.com/kopia/kopia/snapshot/image/recoverymedia"
)

type commandImageDrivers struct {
	jo  jsonOutput
	out textOutput
}

func (c *commandImageDrivers) setup(svc appServices, parent commandParent) {
	cmd := parent.Command("drivers", "List the storage, network and USB drivers that the images of each computer store, which recovery media needs to restore it.")
	c.jo.setup(svc, cmd)
	cmd.Action(svc.repositoryReaderAction(c.run))
	c.out.setup(svc)
}

func (c *commandImageDrivers) run(ctx context.Context, rep repo.Repository) error {
	inv, err := image.DriverInventory(ctx, rep)
	if err != nil {
		return err //nolint:wrapcheck
	}

	if c.jo.jsonOutput {
		if inv == nil {
			inv = []image.HostDrivers{}
		}

		c.out.printStdout("%s\n", c.jo.jsonIndentedBytes(inv, "  "))

		return nil
	}

	if len(inv) == 0 {
		c.out.printStderr("No image stores drivers yet: images store them from this version of kopia on.\n")
		return nil
	}

	// the drivers on this computer's recovery media, when known
	var media []image.Driver
	if r, err := recoverymedia.ReadDefaultRecord(); err == nil {
		media = r.Drivers
	}

	for _, h := range inv {
		c.out.printStdout("%v, image of %v:\n", h.Host, formatTimestamp(h.Image.StartTime.ToTime()))

		for _, d := range h.Drivers {
			mark := ""
			if media != nil && !slices.ContainsFunc(media, func(m image.Driver) bool { return image.SameDriver(m, d) }) {
				mark = "  (not on this computer's recovery media)"
			}

			c.out.printStdout("  %-12v %-32v %v%v\n", d.Class, d.INF, d.Version, mark)
		}
	}

	if media != nil {
		c.out.printStdout("Build recovery media for every computer listed with 'kopia image recovery-media create --drivers-from all'.\n")
	}

	return nil
}

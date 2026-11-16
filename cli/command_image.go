package cli

type commandImage struct {
	check   commandImageCheck
	create  commandImageCreate
	list    commandImageList
	restore commandImageRestore
}

func (c *commandImage) setup(svc advancedAppServices, parent commandParent) {
	cmd := parent.Command("image", "Commands to create, list, check and restore block-level disk images.")
	c.check.setup(svc, cmd)
	c.create.setup(svc, cmd)
	c.list.setup(svc, cmd)
	c.restore.setup(svc, cmd)
}

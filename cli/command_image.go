package cli

type commandImage struct {
	create  commandImageCreate
	list    commandImageList
	restore commandImageRestore
}

func (c *commandImage) setup(svc advancedAppServices, parent commandParent) {
	cmd := parent.Command("image", "Commands to create, list and restore block-level disk images.")
	c.create.setup(svc, cmd)
	c.list.setup(svc, cmd)
	c.restore.setup(svc, cmd)
}

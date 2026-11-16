package cli

type commandImage struct {
	create commandImageCreate
	list   commandImageList
}

func (c *commandImage) setup(svc advancedAppServices, parent commandParent) {
	cmd := parent.Command("image", "Commands to create and list block-level disk images.")
	c.create.setup(svc, cmd)
	c.list.setup(svc, cmd)
}

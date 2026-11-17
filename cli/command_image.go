package cli

type commandImage struct {
	check    commandImageCheck
	recover  commandImageRecover
	media    commandImageRecoveryMedia
	create   commandImageCreate
	list     commandImageList
	ls       commandImageLs
	restore  commandImageRestore
	files    commandImageRestoreFiles
	schedule commandImageSchedule
	stop     commandImageStop
}

func (c *commandImage) setup(svc advancedAppServices, parent commandParent) {
	cmd := parent.Command("image", "Commands to create, list, check, browse and restore block-level disk images.")
	c.check.setup(svc, cmd)
	c.recover.setup(svc, cmd)
	c.media.setup(svc, cmd)
	c.create.setup(svc, cmd)
	c.list.setup(svc, cmd)
	c.ls.setup(svc, cmd)
	c.restore.setup(svc, cmd)
	c.files.setup(svc, cmd)
	c.schedule.setup(svc, cmd)
	c.stop.setup(svc, cmd)
}

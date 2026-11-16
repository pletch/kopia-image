package main

import (
	"errors"
	"log/slog"

	"github.com/wailsapp/wails/v3/pkg/application"
)

// errElevationCanceled is returned when the person declines the
// administrator prompt.
var errElevationCanceled = errors.New("the administrator prompt was declined")

// createRecoveryMedia opens an administrator console that runs
// "kopia image recovery-media create" for a repository. The command lists
// the USB disks, asks which one to erase and shows its progress; building
// the media needs an administrator, which KopiaUI does not run as.
func (s *shell) createRecoveryMedia(repoID string) {
	args := []string{
		"--config-file", s.configs.configFile(repoID),
		"image", "recovery-media", "create",
	}

	err := runElevatedConsole(s.kopiaPath, args)
	if err == nil || errors.Is(err, errElevationCanceled) {
		return
	}

	slog.Warn("unable to start recovery media creation", "repo", repoID, "err", err)

	s.app.Dialog.Error().
		SetTitle(appName).
		SetMessage("Unable to start creating recovery media:\n\n" + err.Error()).
		Show()
}

const recoveryMediaLabel = "Create Recovery Media..."

// addRecoveryMediaMenu adds the item that creates recovery media, on Windows
// and once a repository is configured: one item for one repository, a
// submenu naming each repository otherwise.
func (s *shell) addRecoveryMediaMenu(menu *application.Menu) {
	ids := s.configs.list()
	if !isWindows || len(ids) == 0 {
		return
	}

	const tooltip = "Make a USB stick that restores this computer's disk images; asks for administrator rights"

	if len(ids) == 1 {
		id := ids[0]

		menu.Add(recoveryMediaLabel).
			SetTooltip(tooltip).
			OnClick(func(*application.Context) { s.createRecoveryMedia(id) })

		return
	}

	sub := menu.AddSubmenu(recoveryMediaLabel)

	for _, id := range ids {
		repoID := id
		label := repoID

		if srv := s.server(repoID); srv != nil {
			label = srv.describe()
		}

		sub.Add(label).
			SetTooltip(tooltip).
			OnClick(func(*application.Context) { s.createRecoveryMedia(repoID) })
	}
}

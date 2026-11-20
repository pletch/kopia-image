package cli

import (
	"golang.org/x/sys/windows"
)

// keepConsoleRunning turns off QuickEdit in the console window, if kopia has
// one, and returns a function that turns it back on. With QuickEdit, a click
// in the window starts selecting text, and Windows stops every program that
// prints to the window until the selection ends: a long command such as
// building recovery media seemed to hang until someone pressed Enter.
func keepConsoleRunning() func() {
	h, err := windows.GetStdHandle(windows.STD_INPUT_HANDLE)
	if err != nil || h == 0 || h == windows.InvalidHandle {
		return func() {}
	}

	var mode uint32

	// not a console, as when the task scheduler runs kopia
	if windows.GetConsoleMode(h, &mode) != nil || mode&windows.ENABLE_QUICK_EDIT_MODE == 0 {
		return func() {}
	}

	// QuickEdit changes only together with ENABLE_EXTENDED_FLAGS
	if windows.SetConsoleMode(h, mode&^windows.ENABLE_QUICK_EDIT_MODE|windows.ENABLE_EXTENDED_FLAGS) != nil {
		return func() {}
	}

	return func() {
		_ = windows.SetConsoleMode(h, mode|windows.ENABLE_EXTENDED_FLAGS)
	}
}

//go:build !windows

package main

import (
	"errors"
	"os/exec"
	"time"
)

func hideConsoleWindow(*exec.Cmd) {}

func electronKopiaUIRunning() bool { return false }

// runElevatedConsole is used only on Windows.
func runElevatedConsole(string, []string) error {
	return errors.New("recovery media can be created only on Windows")
}

// runElevatedHidden is used only on Windows.
func runElevatedHidden(string, []string, time.Duration) (uint32, error) {
	return 0, errors.New("administrator rights can be requested only on Windows")
}

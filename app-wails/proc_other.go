//go:build !windows

package main

import (
	"errors"
	"os/exec"
)

func hideConsoleWindow(*exec.Cmd) {}

func electronKopiaUIRunning() bool { return false }

// runElevatedConsole is used only on Windows.
func runElevatedConsole(string, []string) error {
	return errors.New("recovery media can be created only on Windows")
}

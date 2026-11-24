//go:build !windows

package main

import (
	"errors"
	"fmt"
	"os"
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

// kopiaStateDir is used only on Windows.
func kopiaStateDir() (string, error) {
	return "", errors.New("disk images are scheduled only on Windows")
}

// runElevatedDetached is used only on Windows.
func runElevatedDetached(string, []string) error {
	return errors.New("disk images are created only on Windows")
}

// showMessage writes the message to stderr: message boxes are used only on Windows.
func showMessage(title, text string) {
	fmt.Fprintln(os.Stderr, title+": "+text)
}

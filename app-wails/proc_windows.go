//go:build windows

package main

import (
	"os/exec"
	"strings"
	"syscall"
)

const createNoWindow = 0x08000000

// hideConsoleWindow prevents kopia.exe (a console program) from opening a
// console window when started from this GUI process.
func hideConsoleWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
}

// electronKopiaUIRunning reports whether the Electron KopiaUI (KopiaUI.exe) is running.
func electronKopiaUIRunning() bool {
	cmd := exec.Command("tasklist", "/FI", "IMAGENAME eq KopiaUI.exe", "/NH", "/FO", "CSV")
	hideConsoleWindow(cmd)

	out, err := cmd.Output()
	if err != nil {
		return false
	}

	return strings.Contains(strings.ToLower(string(out)), `"kopiaui.exe"`)
}

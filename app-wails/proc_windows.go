//go:build windows

package main

import (
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/windows"
)

const createNoWindow = 0x08000000

// hideConsoleWindow prevents kopia.exe (a console program) from opening a
// console window when started from this GUI process.
func hideConsoleWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
}

// electronKopiaUIRunning reports whether the Electron KopiaUI (KopiaUI.exe) is running.
func electronKopiaUIRunning() bool {
	// the tool from the Windows system folder, not one of that name on PATH
	system32, err := windows.GetSystemDirectory()
	if err != nil {
		return false
	}

	cmd := exec.Command(filepath.Join(system32, "tasklist.exe"), "/FI", "IMAGENAME eq KopiaUI.exe", "/NH", "/FO", "CSV") //nolint:gosec
	hideConsoleWindow(cmd)

	out, err := cmd.Output()
	if err != nil {
		return false
	}

	return strings.Contains(strings.ToLower(string(out)), `"kopiaui.exe"`)
}

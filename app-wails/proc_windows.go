//go:build windows

package main

import (
	"errors"
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

// runElevatedConsole runs a console program as an administrator, after the
// UAC prompt, in a console window that stays open when it ends so that its
// output can be read.
func runElevatedConsole(exe string, args []string) error {
	system32, err := windows.GetSystemDirectory()
	if err != nil {
		return err //nolint:wrapcheck
	}

	parts := []string{syscall.EscapeArg(exe)}
	for _, a := range args {
		parts = append(parts, syscall.EscapeArg(a))
	}

	// cmd /k removes the outermost quotes of the command it is given
	params := `/k "` + strings.Join(parts, " ") + `"`

	verb, _ := windows.UTF16PtrFromString("runas")
	file, _ := windows.UTF16PtrFromString(filepath.Join(system32, "cmd.exe"))
	cmdArgs, _ := windows.UTF16PtrFromString(params)
	dir, _ := windows.UTF16PtrFromString(filepath.Dir(exe))

	err = windows.ShellExecute(0, verb, file, cmdArgs, dir, windows.SW_SHOWNORMAL)
	if errors.Is(err, windows.ERROR_CANCELLED) { //nolint:misspell // the Windows API name
		return errElevationCanceled
	}

	return err //nolint:wrapcheck
}

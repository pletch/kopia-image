//go:build windows

package main

import (
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"

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

var procShellExecuteExW = windows.NewLazySystemDLL("shell32.dll").NewProc("ShellExecuteExW")

// shellExecuteInfo is SHELLEXECUTEINFOW.
type shellExecuteInfo struct {
	cbSize        uint32
	fMask         uint32
	hwnd          windows.HWND
	verb          *uint16
	file          *uint16
	parameters    *uint16
	directory     *uint16
	show          int32
	instApp       windows.Handle
	idList        uintptr
	class         *uint16
	keyClass      windows.Handle
	hotKey        uint32
	iconOrMonitor windows.Handle
	process       windows.Handle
}

const (
	seeMaskNoCloseProcess = 0x00000040
	seeMaskNoAsync        = 0x00000100
)

// runElevatedHidden runs a program as an administrator, after the UAC
// prompt, without a window, and returns its exit code. It waits at most
// timeout for the program to end once it started.
func runElevatedHidden(exe string, args []string, timeout time.Duration) (uint32, error) {
	parts := make([]string, 0, len(args))
	for _, a := range args {
		parts = append(parts, syscall.EscapeArg(a))
	}

	verb, _ := windows.UTF16PtrFromString("runas")
	file, err := windows.UTF16PtrFromString(exe)
	if err != nil {
		return 0, err //nolint:wrapcheck
	}

	params, err := windows.UTF16PtrFromString(strings.Join(parts, " "))
	if err != nil {
		return 0, err //nolint:wrapcheck
	}

	dir, _ := windows.UTF16PtrFromString(filepath.Dir(exe))

	info := &shellExecuteInfo{
		fMask:      seeMaskNoCloseProcess | seeMaskNoAsync,
		verb:       verb,
		file:       file,
		parameters: params,
		directory:  dir,
		show:       windows.SW_HIDE,
	}
	info.cbSize = uint32(unsafe.Sizeof(*info))

	if ok, _, callErr := procShellExecuteExW.Call(uintptr(unsafe.Pointer(info))); ok == 0 {
		if errors.Is(callErr, windows.ERROR_CANCELLED) { //nolint:misspell // the Windows API name
			return 0, errElevationCanceled
		}

		return 0, fmt.Errorf("unable to start %v: %w", filepath.Base(exe), callErr)
	}

	if info.process == 0 {
		return 0, fmt.Errorf("unable to follow %v", filepath.Base(exe))
	}

	defer windows.CloseHandle(info.process) //nolint:errcheck

	ev, err := windows.WaitForSingleObject(info.process, uint32(min(timeout.Milliseconds(), windows.INFINITE-1))) //nolint:gosec // clamped
	if err != nil {
		return 0, fmt.Errorf("unable to wait for %v: %w", filepath.Base(exe), err)
	}

	if ev != windows.WAIT_OBJECT_0 {
		return 0, fmt.Errorf("%v did not end within %v", filepath.Base(exe), timeout)
	}

	var code uint32
	if err := windows.GetExitCodeProcess(info.process, &code); err != nil {
		return 0, fmt.Errorf("unable to read the exit code of %v: %w", filepath.Base(exe), err)
	}

	return code, nil
}

// kopiaStateDir returns ProgramData\kopia, where kopia image records what
// it did, readable by every user.
func kopiaStateDir() (string, error) {
	pd, err := windows.KnownFolderPath(windows.FOLDERID_ProgramData, 0)
	if err != nil {
		return "", fmt.Errorf("unable to find ProgramData: %w", err)
	}

	return filepath.Join(pd, "kopia"), nil
}

// runElevatedDetached starts a program as an administrator, after the UAC
// prompt, without a window, and does not wait for it: for long jobs that
// report their progress elsewhere, such as an image run.
func runElevatedDetached(exe string, args []string) error {
	parts := make([]string, 0, len(args))
	for _, a := range args {
		parts = append(parts, syscall.EscapeArg(a))
	}

	verb, _ := windows.UTF16PtrFromString("runas")

	file, err := windows.UTF16PtrFromString(exe)
	if err != nil {
		return err //nolint:wrapcheck
	}

	params, err := windows.UTF16PtrFromString(strings.Join(parts, " "))
	if err != nil {
		return err //nolint:wrapcheck
	}

	dir, _ := windows.UTF16PtrFromString(filepath.Dir(exe))

	info := &shellExecuteInfo{
		fMask:      seeMaskNoAsync,
		verb:       verb,
		file:       file,
		parameters: params,
		directory:  dir,
		show:       windows.SW_HIDE,
	}
	info.cbSize = uint32(unsafe.Sizeof(*info))

	if ok, _, callErr := procShellExecuteExW.Call(uintptr(unsafe.Pointer(info))); ok == 0 {
		if errors.Is(callErr, windows.ERROR_CANCELLED) { //nolint:misspell // the Windows API name
			return errElevationCanceled
		}

		return fmt.Errorf("unable to start %v: %w", filepath.Base(exe), callErr)
	}

	return nil
}

//go:build windows

package main

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	webviewProfileWait     = 10 * time.Second
	webviewProfileKillWait = 5 * time.Second
	webviewPollInterval    = 250 * time.Millisecond

	// ProcessCommandLineInformation (Windows 8.1+) for NtQueryInformationProcess.
	processCommandLineInformation = 60
)

// webviewDataDir is the WebView2 profile directory used by this app.
func webviewDataDir() string {
	base, err := os.UserCacheDir() // %LOCALAPPDATA%
	if err != nil {
		return ""
	}

	return filepath.Join(base, "KopiaUI-Wails", "WebView2")
}

// ensureWebViewProfileFree waits for WebView2 processes left behind by a
// previous instance to release the profile. When the app is force-killed its
// WebView2 browser process lingers for a few seconds and keeps the profile
// locked, and creating a new WebView2 controller on it fails with
// ERROR_BUSY (0x800700AA), which Wails treats as fatal.
//
// It must only be called by the primary instance (after the single instance
// check), because then any WebView2 process using this profile is an orphan.
func ensureWebViewProfileFree(dir string) {
	if dir == "" {
		return
	}

	lockFile := filepath.Join(dir, "EBWebView", "lockfile")

	if !fileLocked(lockFile) {
		return
	}

	slog.Info("waiting for previous WebView2 processes to release the profile", "dir", dir)

	if waitUnlocked(lockFile, webviewProfileWait) {
		return
	}

	killed := killOrphanWebViews(dir)
	slog.Warn("terminated WebView2 processes left behind by a previous instance", "count", killed)

	if !waitUnlocked(lockFile, webviewProfileKillWait) {
		slog.Error("WebView2 profile is still in use; windows may fail to open", "dir", dir)
	}
}

func waitUnlocked(path string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		if !fileLocked(path) {
			return true
		}

		time.Sleep(webviewPollInterval)
	}

	return !fileLocked(path)
}

// fileLocked reports whether another process has the file open. Chromium keeps
// its profile "lockfile" open for as long as the browser process runs.
func fileLocked(path string) bool {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return false
	}

	h, err := windows.CreateFile(p, windows.GENERIC_READ|windows.GENERIC_WRITE, 0, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return errors.Is(err, windows.ERROR_SHARING_VIOLATION) || errors.Is(err, windows.ERROR_ACCESS_DENIED)
	}

	_ = windows.CloseHandle(h)

	return false
}

// killOrphanWebViews terminates WebView2 browser processes whose command line
// references this app's profile directory. Renderer and helper processes exit
// with their browser process.
func killOrphanWebViews(dir string) int {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return 0
	}
	defer windows.CloseHandle(snap) //nolint:errcheck

	needle := strings.ToLower(dir)
	killed := 0

	var pe windows.ProcessEntry32
	pe.Size = uint32(unsafe.Sizeof(pe))

	for err = windows.Process32First(snap, &pe); err == nil; err = windows.Process32Next(snap, &pe) {
		if !strings.EqualFold(windows.UTF16ToString(pe.ExeFile[:]), "msedgewebview2.exe") {
			continue
		}

		cmdline := strings.ToLower(processCommandLine(pe.ProcessID))
		if !strings.Contains(cmdline, needle) || strings.Contains(cmdline, "--type=") {
			continue // another app's WebView2, or a child process
		}

		h, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, pe.ProcessID)
		if err != nil {
			continue
		}

		if windows.TerminateProcess(h, 1) == nil {
			killed++
		}

		_ = windows.CloseHandle(h)
	}

	return killed
}

func processCommandLine(pid uint32) string {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return ""
	}
	defer windows.CloseHandle(h) //nolint:errcheck

	buf := make([]byte, 64*1024)

	var retLen uint32

	if err := windows.NtQueryInformationProcess(h, processCommandLineInformation, unsafe.Pointer(&buf[0]), uint32(len(buf)), &retLen); err != nil {
		return ""
	}

	us := (*windows.NTUnicodeString)(unsafe.Pointer(&buf[0]))
	if us.Buffer == nil || us.Length == 0 {
		return ""
	}

	return windows.UTF16ToString(unsafe.Slice(us.Buffer, us.Length/2))
}

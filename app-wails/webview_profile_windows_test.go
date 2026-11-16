//go:build windows

package main

import "testing"

func TestCommandLineUsesProfile(t *testing.T) {
	const dir = `c:\users\tim\appdata\local\kopiaui-wails\webview2`

	uses := []string{
		`"c:\program files (x86)\microsoft\edgewebview\msedgewebview2.exe" --user-data-dir="` + dir + `\ebwebview" --lang=en`,
		`msedgewebview2.exe --user-data-dir=` + dir + `\ebwebview`,
		`msedgewebview2.exe --user-data-dir=` + dir + ` --lang=en`,
		`msedgewebview2.exe --user-data-dir="` + dir + `"`,
		`msedgewebview2.exe --user-data-dir=` + dir,
		// a similar profile earlier on the line does not hide the real one
		`msedgewebview2.exe --x=` + dir + `-old --user-data-dir=` + dir + `\ebwebview`,
	}

	for _, cmdline := range uses {
		if !commandLineUsesProfile(cmdline, dir) {
			t.Errorf("not recognized: %v", cmdline)
		}
	}

	others := []string{
		``,
		`msedgewebview2.exe --user-data-dir="c:\users\tim\appdata\local\otherapp\webview2\ebwebview"`,
		// profiles of other programs whose path merely starts the same way
		`msedgewebview2.exe --user-data-dir="` + dir + `-old\ebwebview"`,
		`msedgewebview2.exe --user-data-dir="` + dir + `2\ebwebview"`,
		`msedgewebview2.exe --user-data-dir=` + dir + `.bak`,
	}

	for _, cmdline := range others {
		if commandLineUsesProfile(cmdline, dir) {
			t.Errorf("another program's WebView2 would be terminated: %v", cmdline)
		}
	}

	if commandLineUsesProfile(`msedgewebview2.exe --user-data-dir=c:\x`, "") {
		t.Error("an empty profile directory matches nothing")
	}

	if !commandLineUsesProfile(`msedgewebview2.exe --user-data-dir=`+dir+`\ebwebview`, dir+`\`) {
		t.Error("a trailing separator on the profile directory is ignored")
	}
}

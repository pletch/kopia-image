package server

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func Test_strictPathEscape(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"/webdav/all/", "/webdav/all/"},
		{"a-b_c.d~e", "a-b_c.d~e"},
		{"dir with spaces", "dir%20with%20spaces"},
		{`a&b'c"d%e!f`, "a%26b%27c%22d%25e%21f"},
		{"café", "caf%C3%A9"},
	}

	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			require.Equal(t, tc.want, strictPathEscape(tc.in))
		})
	}
}

func Test_netUseUserArg(t *testing.T) {
	cases := []struct {
		user string
		want string
	}{
		{"kopia", " /user:kopia"},
		{"user@host", " /user:user@host"},
		{"", ""},
		{"bad user", ""},
		{`a"&b`, ""},
	}

	for _, tc := range cases {
		t.Run(tc.user, func(t *testing.T) {
			require.Equal(t, tc.want, netUseUserArg(tc.user))
		})
	}
}

func Test_windowsMountScript_escapesPercent(t *testing.T) {
	script := windowsMountScript("https://h/webdav/all/a%20b/", "kopia")

	require.Contains(t, script, `net use %DRIVE% "https://h/webdav/all/a%%20b/" /user:kopia /persistent:no`)
}

func Test_windowsMountScript_showsDisconnect(t *testing.T) {
	require.Contains(t, windowsMountScript("https://h/webdav/all/", "kopia"), "echo     net use %DRIVE% /delete")
}

func Test_linuxMountScript_showsUnmount(t *testing.T) {
	require.Contains(t, linuxMountScript("davs://h/webdav/all/"), `echo "To unmount: gio mount -u '$url'"`)
}

func Test_windowsMountScript_skipsRememberedNetworkDrives(t *testing.T) {
	require.Contains(t, windowsMountScript("https://h/webdav/all/", "kopia"),
		`if not exist %%L:\ (net use %%L: >nul 2>&1 || set DRIVE=%%L:)`)
}

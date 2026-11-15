//go:build windows

package mount

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows/registry"
)

func Test_mountPoints2KeyName(t *testing.T) {
	cases := []struct {
		url  string
		want string
	}{
		{"http://127.0.0.1:54321", "##127.0.0.1@54321#DavWWWRoot"},
		{"http://127.0.0.1:54321/", "##127.0.0.1@54321#DavWWWRoot"},
		{"http://localhost", "##localhost#DavWWWRoot"},
	}

	for _, tc := range cases {
		t.Run(tc.url, func(t *testing.T) {
			got, err := mountPoints2KeyName(tc.url)
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

func Test_mountPoints2KeyName_unsupported(t *testing.T) {
	for _, u := range []string{
		"https://127.0.0.1:54321",
		"http://127.0.0.1:54321/some/path",
		"127.0.0.1:54321",
		"http://",
		"://bad",
	} {
		t.Run(u, func(t *testing.T) {
			_, err := mountPoints2KeyName(u)
			require.Error(t, err)
		})
	}
}

func Test_driveLabel_setAndRemove(t *testing.T) {
	keyName := fmt.Sprintf("##kopia-test-%d@1#%s", time.Now().UnixNano(), webdavRootFolder)
	keyPath := mountPoints2Key + `\` + keyName

	t.Cleanup(func() {
		registry.DeleteKey(registry.CURRENT_USER, keyPath) //nolint:errcheck
	})

	const label = "Kopia - Documents - 2026-11-15 10:02"

	require.NoError(t, setDriveLabel(keyName, label))

	k, err := registry.OpenKey(registry.CURRENT_USER, keyPath, registry.QUERY_VALUE)
	require.NoError(t, err)

	got, _, err := k.GetStringValue(driveLabelValue)
	require.NoError(t, k.Close())
	require.NoError(t, err)
	require.Equal(t, label, got)

	require.NoError(t, removeDriveLabel(keyName))

	_, err = registry.OpenKey(registry.CURRENT_USER, keyPath, registry.QUERY_VALUE)
	require.ErrorIs(t, err, registry.ErrNotExist)

	// removing a label that is already gone is not an error
	require.NoError(t, removeDriveLabel(keyName))
}

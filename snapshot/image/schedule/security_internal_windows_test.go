package schedule

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

const usersCanRead = "(A;;FR;;;AU)"

func installTestTask(t *testing.T, name string) {
	t.Helper()

	require.NoError(t, Install(t.Context(), &Task{
		Name:    name,
		Command: `C:\Windows\System32\cmd.exe`,
		At:      time.Date(2000, 1, 1, 3, 0, 0, 0, time.Local), //nolint:gosmopolitan
	}))
}

func requireElevated(t *testing.T) {
	t.Helper()

	if !windows.GetCurrentProcessToken().IsElevated() {
		t.Skip("registering a task that runs as SYSTEM requires administrator")
	}
}

// TestInstall_replacesPermissions checks that installing over a task kopia
// registered before lets users read it: schtasks /Create /F alone keeps the
// permissions of the task it replaces.
func TestInstall_replacesPermissions(t *testing.T) {
	requireElevated(t)

	name := `\KopiaTest\Install permissions`

	t.Cleanup(func() { _ = Delete(context.Background(), name) })

	installTestTask(t, name)
	require.NoError(t, setTaskSecurityDescriptor(name, "D:(A;;FA;;;BA)(A;;FA;;;SY)"))

	sddl, err := getTaskSecurityDescriptor(name)
	require.NoError(t, err)
	require.NotContains(t, sddl, usersCanRead)

	installTestTask(t, name)

	sddl, err = getTaskSecurityDescriptor(name)
	require.NoError(t, err)
	require.Contains(t, sddl, usersCanRead)
}

// TestShareStatus gives a task registered without read access for users the
// permissions of new tasks.
func TestShareStatus(t *testing.T) {
	requireElevated(t)

	name := `\KopiaTest\Share status`

	t.Cleanup(func() { _ = Delete(context.Background(), name) })

	installTestTask(t, name)
	require.NoError(t, setTaskSecurityDescriptor(name, "D:(A;;FA;;;BA)(A;;FA;;;SY)"))

	require.NoError(t, ShareStatus(t.Context(), name))

	sddl, err := getTaskSecurityDescriptor(name)
	require.NoError(t, err)
	require.Contains(t, sddl, usersCanRead)

	require.ErrorIs(t, ShareStatus(t.Context(), `\KopiaTest\No such task`), ErrTaskNotFound)
}

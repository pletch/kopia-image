package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

// TestInstallTaskExecutable_whileRunning checks that the tasks' copy of kopia
// can be replaced while an image run uses it, as when the tasks are updated
// during a long first image.
func TestInstallTaskExecutable_whileRunning(t *testing.T) {
	loc := &imageScheduleLocation{configDir: t.TempDir()}
	running := loc.taskExe()

	system32, err := windows.GetSystemDirectory()
	require.NoError(t, err)

	ping, err := os.ReadFile(filepath.Join(system32, "PING.EXE"))
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(running), 0o700))
	require.NoError(t, os.WriteFile(running, ping, 0o700))

	// a program running from the tasks' copy
	cmd := exec.CommandContext(t.Context(), running, "-n", "30", "127.0.0.1")
	require.NoError(t, cmd.Start())

	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	newExe := filepath.Join(t.TempDir(), "kopia.exe")
	require.NoError(t, os.WriteFile(newExe, []byte("new kopia"), 0o600))

	require.NoError(t, installTaskExecutable(loc, newExe))

	b, err := os.ReadFile(running)
	require.NoError(t, err)
	require.Equal(t, "new kopia", string(b))

	// the running program goes on from the renamed file
	require.Nil(t, cmd.ProcessState)
	require.FileExists(t, running+".old")

	// once it ends, the next replacement removes the renamed file
	require.NoError(t, cmd.Process.Kill())
	_ = cmd.Wait()

	require.NoError(t, installTaskExecutable(loc, newExe))
	require.NoFileExists(t, running+".old")
}

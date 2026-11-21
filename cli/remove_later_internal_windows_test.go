package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

// TestRemoveAtRestart leaves a running program for Windows to remove when it
// restarts.
func TestRemoveAtRestart(t *testing.T) {
	if !windows.GetCurrentProcessToken().IsElevated() {
		t.Skip("marking files for removal at restart requires administrator")
	}

	dir := filepath.Join(t.TempDir(), "image")
	bin := filepath.Join(dir, "bin")
	require.NoError(t, os.MkdirAll(bin, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "repository.config"), []byte("{}"), 0o600))

	system32, err := windows.GetSystemDirectory()
	require.NoError(t, err)

	exe := filepath.Join(bin, "kopia.exe")
	require.NoError(t, copyFile(filepath.Join(system32, "PING.EXE"), exe))

	cmd := exec.CommandContext(t.Context(), exe, "-n", "60", "127.0.0.1")
	require.NoError(t, cmd.Start())

	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	require.Error(t, os.RemoveAll(dir), "a running program cannot be removed")
	require.NoFileExists(t, filepath.Join(dir, "repository.config"))

	left, err := removeAtRestart(dir)
	require.NoError(t, err)
	require.Equal(t, []string{bin, exe}, left)
}

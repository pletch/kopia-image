package cli_test

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/snapshot/image/schedule"
	"github.com/kopia/kopia/tests/testenv"
)

// TestImageScheduleInstall_recordsFailure checks that a request ID records
// why install failed even when it fails before the command runs, as when
// the repository does not open: KopiaUI runs install without a window and
// can only read the outcome.
func TestImageScheduleInstall_recordsFailure(t *testing.T) {
	env := testenv.NewCLITest(t, testenv.RepoFormatNotImportant, testenv.NewInProcRunner(t))

	stateDir := t.TempDir()
	statePath := filepath.Join(stateDir, "state.json")

	// not connected to a repository
	env.RunAndExpectFailure(t, "image", "schedule", "install", "C:",
		"--task-folder", `\KopiaTest`, "--config-dir", filepath.Join(t.TempDir(), "image"), "--state-file", statePath,
		"--request-id", "req-42")

	req, err := schedule.ReadRequest(schedule.RequestPath(stateDir))
	require.NoError(t, err)
	require.Equal(t, "req-42", req.ID)
	require.Equal(t, "install", req.Operation)
	require.False(t, req.Success)
	require.NotEmpty(t, req.Error)

	// without a request ID nothing is recorded
	other := t.TempDir()
	env.RunAndExpectFailure(t, "image", "schedule", "install", "C:", "--state-file", filepath.Join(other, "state.json"))
	require.NoFileExists(t, schedule.RequestPath(other))
}

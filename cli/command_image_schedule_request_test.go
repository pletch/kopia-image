package cli_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/internal/clock"
	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/snapshot/image/disksource"
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

// TestImageScheduleUpdate checks that update reports whether the tasks run
// another kopia and replaces their copy, keeping their connection.
func TestImageScheduleUpdate(t *testing.T) {
	env := testenv.NewCLITest(t, testenv.RepoFormatNotImportant, testenv.NewInProcRunner(t))

	stateDir := t.TempDir()
	configDir := filepath.Join(t.TempDir(), "image")
	loc := []string{"--task-folder", `\KopiaTest`, "--config-dir", configDir, "--state-file", filepath.Join(stateDir, "state.json")}

	// not scheduled
	out := env.RunAndExpectSuccess(t, append([]string{"image", "schedule", "update", "--check"}, loc...)...)
	require.Equal(t, []string{"not-scheduled"}, out)

	env.RunAndExpectFailure(t, append([]string{"image", "schedule", "update"}, loc...)...)

	// scheduled by an older kopia
	require.NoError(t, schedule.WriteSettings(schedule.SettingsPath(stateDir), &schedule.Settings{
		Volumes: []string{"C:"}, At: "02:00", KopiaVersion: "0.0.1-old",
	}))

	out = env.RunAndExpectSuccess(t, append([]string{"image", "schedule", "update", "--check"}, loc...)...)
	require.Equal(t, []string{"outdated 0.0.1-old " + repo.BuildVersion}, out)

	// the tasks' connection is missing: install is needed
	env.RunAndExpectFailure(t, append([]string{"image", "schedule", "update", "--request-id", "u1"}, loc...)...)

	req, err := schedule.ReadRequest(schedule.RequestPath(stateDir))
	require.NoError(t, err)
	require.Equal(t, "u1", req.ID)
	require.False(t, req.Success)
	require.Contains(t, req.Error, "kopia image schedule install")

	// with the connection, the copy is replaced and the connection kept
	require.NoError(t, os.MkdirAll(configDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(configDir, "repository.config"), []byte("{}"), 0o600))

	newExe := filepath.Join(t.TempDir(), "kopia.exe")
	require.NoError(t, os.WriteFile(newExe, []byte("new kopia"), 0o600))

	env.RunAndExpectSuccess(t, append([]string{"image", "schedule", "update", "--kopia-exe", newExe, "--request-id", "u2"}, loc...)...)

	b, err := os.ReadFile(filepath.Join(configDir, "bin", "kopia.exe"))
	require.NoError(t, err)
	require.Equal(t, "new kopia", string(b))

	cfg, err := os.ReadFile(filepath.Join(configDir, "repository.config"))
	require.NoError(t, err)
	require.Equal(t, "{}", string(cfg))

	s, err := schedule.ReadSettings(schedule.SettingsPath(stateDir))
	require.NoError(t, err)
	require.Equal(t, repo.BuildVersion, s.KopiaVersion)
	require.Equal(t, []string{"C:"}, s.Volumes)

	req, err = schedule.ReadRequest(schedule.RequestPath(stateDir))
	require.NoError(t, err)
	require.Equal(t, "u2", req.ID)
	require.True(t, req.Success, req.Error)

	out = env.RunAndExpectSuccess(t, append([]string{"image", "schedule", "update", "--check"}, loc...)...)
	require.Equal(t, []string{"up-to-date " + repo.BuildVersion}, out)
}

// TestImageSchedulePause_notScheduled checks that pause and resume say when
// there is nothing to pause, and record it for KopiaUI.
func TestImageSchedulePause_notScheduled(t *testing.T) {
	env := testenv.NewCLITest(t, testenv.RepoFormatNotImportant, testenv.NewInProcRunner(t))

	stateDir := t.TempDir()
	loc := []string{"--task-folder", `\KopiaTestNone`, "--config-dir", filepath.Join(t.TempDir(), "image"), "--state-file", filepath.Join(stateDir, "state.json")}

	for _, op := range []string{"pause", "resume"} {
		_, stderr := env.RunAndExpectFailure(t, append([]string{"image", "schedule", op, "--request-id", op}, loc...)...)
		require.Contains(t, strings.Join(stderr, "\n"), "not scheduled")

		req, err := schedule.ReadRequest(schedule.RequestPath(stateDir))
		require.NoError(t, err)
		require.Equal(t, op, req.ID)
		require.Equal(t, op, req.Operation)
		require.False(t, req.Success)
	}
}

// TestImageStop checks that stop asks the run in progress to stop, and says
// when no image is being created.
func TestImageStop(t *testing.T) {
	env := testenv.NewCLITest(t, testenv.RepoFormatNotImportant, testenv.NewInProcRunner(t))

	stateDir := t.TempDir()
	loc := []string{"--state-file", filepath.Join(stateDir, "state.json")}

	_, stderr := env.RunAndExpectFailure(t, append([]string{"image", "stop", "--request-id", "s1"}, loc...)...)
	require.Contains(t, strings.Join(stderr, "\n"), "no image is being created")
	require.NoFileExists(t, disksource.StopPathIn(stateDir))

	req, err := schedule.ReadRequest(schedule.RequestPath(stateDir))
	require.NoError(t, err)
	require.Equal(t, "s1", req.ID)
	require.False(t, req.Success)

	// a run in progress
	require.NoError(t, disksource.WriteRunProgress(disksource.ProgressPathIn(stateDir), &disksource.RunProgress{UpdatedAt: clock.Now()}))

	env.RunAndExpectSuccess(t, append([]string{"image", "stop"}, loc...)...)

	r, err := disksource.ReadStopRequest(disksource.StopPathIn(stateDir))
	require.NoError(t, err)
	require.False(t, r.RequestedAt.IsZero())
}

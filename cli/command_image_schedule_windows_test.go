package cli_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/internal/vhdxtest"
	"github.com/kopia/kopia/snapshot/image/disksource"
	"github.com/kopia/kopia/snapshot/image/schedule"
	"github.com/kopia/kopia/snapshot/image/securedir"
	"github.com/kopia/kopia/tests/testenv"
)

const testTaskFolder = `\KopiaTest`

// buildKopia builds the kopia executable for scheduled tasks to run.
func buildKopia(t *testing.T) string {
	t.Helper()

	exe := filepath.Join(t.TempDir(), "kopia.exe")

	out, err := exec.CommandContext(t.Context(), "go", "build", "-o", exe, "github.com/kopia/kopia").CombinedOutput()
	require.NoError(t, err, "go build:\n%s", out)

	return exe
}

func TestImageScheduleRunsAsSystem(t *testing.T) {
	mnt := vhdxtest.New(t).FormatAndMount()
	require.NoError(t, os.WriteFile(filepath.Join(mnt, "hello.txt"), []byte("hello"), 0o600))

	exe := buildKopia(t)

	env := testenv.NewCLITest(t, testenv.RepoFormatNotImportant, testenv.NewInProcRunner(t))
	env.RunAndExpectSuccess(t, "repo", "create", "filesystem", "--path", env.RepoDir)

	configDir := filepath.Join(t.TempDir(), "image")
	statePath := filepath.Join(t.TempDir(), "state.json")
	loc := []string{"--task-folder", testTaskFolder, "--config-dir", configDir, "--state-file", statePath}

	t.Cleanup(func() {
		for _, name := range []string{"Image create", "Image check"} {
			_ = schedule.Delete(context.Background(), testTaskFolder+`\`+name)
		}
	})

	out := env.RunAndExpectSuccess(t, append([]string{"image", "schedule", "install", mnt, "--at", "03:17", "--days", "mon,thu", "--check-day", "sat", "--kopia-exe", exe, "--request-id", "install-1"}, loc...)...)
	t.Logf("install:\n%s", strings.Join(out, "\n"))

	// the settings and the outcome are recorded where the web UI can read them
	stateDir := filepath.Dir(statePath)

	settings, err := schedule.ReadSettings(schedule.SettingsPath(stateDir))
	require.NoError(t, err)
	require.Equal(t, []string{mnt}, settings.Volumes)
	require.Equal(t, "03:17", settings.At)
	require.Equal(t, []string{"Monday", "Thursday"}, settings.Days)
	require.Equal(t, "Saturday", settings.CheckDay)
	require.NotEmpty(t, settings.ConnectionDigest)

	req, err := schedule.ReadRequest(schedule.RequestPath(stateDir))
	require.NoError(t, err)
	require.Equal(t, "install-1", req.ID)
	require.True(t, req.Success, req.Error)

	// the copied connection and password are readable only by SYSTEM and Administrators
	require.FileExists(t, filepath.Join(configDir, "repository.config"))
	require.FileExists(t, filepath.Join(configDir, "repository.config.kopia-password"))

	// the tasks run a copy of kopia that only administrators can replace, and
	// the password file is encrypted for this computer
	require.FileExists(t, filepath.Join(configDir, "bin", "kopia.exe"))

	passwordFile, err := os.ReadFile(filepath.Join(configDir, "repository.config.kopia-password"))
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(string(passwordFile), "dpapi:"), "the password is not stored in the clear")

	dacl, err := securedir.Describe(configDir)
	require.NoError(t, err)
	t.Logf("config folder permissions: %v", dacl)
	require.Contains(t, dacl, "O:BA", "owned by Administrators")
	require.Contains(t, dacl, "D:P")
	require.Contains(t, dacl, "(A;OICI;FA;;;SY)")
	require.Contains(t, dacl, "(A;OICI;FA;;;BA)")
	require.NotContains(t, dacl, ";;;BU)", "no access for ordinary users")

	props, err := schedule.Query(t.Context(), testTaskFolder+`\Image create`)
	require.NoError(t, err)
	require.Contains(t, props["Next Run Time"], "3:17")
	require.Equal(t, "SYSTEM", props["Run As User"])

	// pause and resume keep the settings and tell the web UI
	env.RunAndExpectSuccess(t, append([]string{"image", "schedule", "pause"}, loc...)...)

	props, err = schedule.Query(t.Context(), testTaskFolder+`\Image create`)
	require.NoError(t, err)
	require.Equal(t, "Disabled", props["Scheduled Task State"])

	settings, err = schedule.ReadSettings(schedule.SettingsPath(stateDir))
	require.NoError(t, err)
	require.True(t, settings.Paused)
	require.Equal(t, "03:17", settings.At)

	env.RunAndExpectSuccess(t, append([]string{"image", "schedule", "resume"}, loc...)...)

	props, err = schedule.Query(t.Context(), testTaskFolder+`\Image create`)
	require.NoError(t, err)
	require.Equal(t, "Enabled", props["Scheduled Task State"])

	settings, err = schedule.ReadSettings(schedule.SettingsPath(stateDir))
	require.NoError(t, err)
	require.False(t, settings.Paused)

	// run the task now: Task Scheduler runs kopia as SYSTEM
	env.RunAndExpectSuccess(t, append([]string{"image", "schedule", "run"}, loc...)...)

	statusPath := disksource.StatusPath(statePath)

	var st *disksource.RunStatus

	require.Eventually(t, func() bool {
		st, err = disksource.ReadRunStatus(statusPath)
		return err == nil
	}, 5*time.Minute, 2*time.Second, "the scheduled run did not finish; logs are in %v", filepath.Join(configDir, "logs"))

	if !st.Success {
		dumpLogs(t, filepath.Join(configDir, "logs"))
	}

	require.True(t, st.Success, "scheduled run failed: %v", st.Error)
	require.Len(t, st.Images, 1)

	// the image belongs to the same user@host as the interactive connection
	out = env.RunAndExpectSuccess(t, "image", "list")
	require.Len(t, out, 2)
	require.Contains(t, out[1], st.Images[0].SnapshotID)

	out = env.RunAndExpectSuccess(t, append([]string{"image", "schedule", "show"}, loc...)...)
	t.Logf("show:\n%s", strings.Join(out, "\n"))
	require.Contains(t, strings.Join(out, "\n"), "succeeded")

	env.RunAndExpectSuccess(t, append([]string{"image", "schedule", "remove", "--request-id", "remove-1"}, loc...)...)
	require.NoDirExists(t, configDir)
	require.NoFileExists(t, schedule.SettingsPath(stateDir))

	req, err = schedule.ReadRequest(schedule.RequestPath(stateDir))
	require.NoError(t, err)
	require.Equal(t, "remove-1", req.ID)
	require.True(t, req.Success, req.Error)

	_, err = schedule.Query(t.Context(), testTaskFolder+`\Image create`)
	require.ErrorIs(t, err, schedule.ErrTaskNotFound)
}

func dumpLogs(t *testing.T, dir string) {
	t.Helper()

	_ = filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() {
			if b, rerr := os.ReadFile(p); rerr == nil {
				t.Logf("%v:\n%s", p, b)
			}
		}

		return nil
	})
}

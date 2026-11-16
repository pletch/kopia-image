package cli

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/repo/compression"
	"github.com/kopia/kopia/snapshot/image/disksource"
)

func TestParseWeekdays(t *testing.T) {
	days, err := parseWeekdays("")
	require.NoError(t, err)
	require.Nil(t, days)

	days, err = parseWeekdays("Mon, thursday,SUN")
	require.NoError(t, err)
	require.Equal(t, []time.Weekday{time.Monday, time.Thursday, time.Sunday}, days)

	for _, bad := range []string{"mo", "funday", "Mon,,Tue"} {
		_, err := parseWeekdays(bad)
		require.Error(t, err, bad)
	}
}

func TestImageScheduleInstall_tasks(t *testing.T) {
	c := &commandImageScheduleInstall{
		volumes:       []string{"C:", "D:"},
		at:            "02:15",
		days:          "mon,fri",
		timeLimit:     3 * time.Hour,
		retries:       2,
		retryInterval: 10 * time.Minute,
		wake:          true,
		flushInterval: 5 * time.Minute,
		checkDay:      "Sunday",
		checkAt:       "12:00",
		checkSample:   2.5,
		kopiaExe:      `C:\Program Files\Kopia\kopia.exe`,
		loc: imageScheduleLocation{
			taskFolder: `\Kopia\`,
			configDir:  `C:\ProgramData\kopia\image`,
			statePath:  `C:\ProgramData\kopia\image-shadow-copies.json`,
		},
	}

	create, check, err := c.tasks()
	require.NoError(t, err)

	require.Equal(t, `\Kopia\Image create`, create.Name)
	require.Equal(t, []time.Weekday{time.Monday, time.Friday}, create.Days)
	require.Equal(t, 2, create.At.Hour())
	require.Equal(t, 15, create.At.Minute())
	require.Equal(t, []string{
		"--config-file", filepath.Join(`C:\ProgramData\kopia\image`, "repository.config"),
		"--no-use-credential-manager",
		"--log-dir", filepath.Join(`C:\ProgramData\kopia\image`, "logs"),
		"image", "create", "C:", "D:",
		"--flush-interval", "5m0s",
		"--state-file", `C:\ProgramData\kopia\image-shadow-copies.json`,
	}, create.Arguments)
	require.Equal(t, filepath.Join(`C:\ProgramData\kopia\image`, "bin", "kopia.exe"), create.Command, "the tasks run the copy in the secured folder")
	require.Equal(t, create.Command, check.Command)
	require.Equal(t, 2, create.RetryCount)
	require.True(t, create.WakeToRun)

	require.Equal(t, `\Kopia\Image check`, check.Name)
	require.Equal(t, []time.Weekday{time.Sunday}, check.Days)
	require.Equal(t, []string{"image", "check", "--sample", "2.5"}, check.Arguments[len(check.Arguments)-4:])

	require.False(t, check.WakeToRun)

	c.checkNTFS = true
	_, check, err = c.tasks()
	require.NoError(t, err)
	require.Equal(t, []string{"image", "check", "--sample", "2.5", "--ntfs"}, check.Arguments[len(check.Arguments)-5:])

	c.checkNTFS = false

	c.checkDay = "none"
	_, check, err = c.tasks()
	require.NoError(t, err)
	require.Nil(t, check)

	c.at = "25:00"
	_, _, err = c.tasks()
	require.ErrorContains(t, err, "invalid --at")
}

func TestImageCompressor(t *testing.T) {
	// inherited settings, including the global default "none", are ignored
	require.Equal(t, compression.Name("zstd-fastest"), imageCompressor("none", false))
	require.Equal(t, compression.Name("zstd-fastest"), imageCompressor("s2-default", false))

	// the image source's own setting wins
	require.Equal(t, compression.Name(""), imageCompressor("none", true))
	require.Equal(t, compression.Name("s2-default"), imageCompressor("s2-default", true))
}

func TestRunStatus_roundTrip(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "sub", "state.json")
	path := disksource.StatusPath(statePath)

	uploaded := int64(123)
	want := &disksource.RunStatus{
		Volumes:   []string{"C:"},
		StartTime: time.Date(2026, 11, 16, 2, 0, 0, 0, time.UTC),
		EndTime:   time.Date(2026, 11, 16, 2, 5, 0, 0, time.UTC),
		Images:    []disksource.RunImage{{Source: "tim@desktop:image:disk:x", SnapshotID: "k1", BytesRead: 9, UploadedBytes: &uploaded}},
		Error:     "boom",
	}

	require.NoError(t, disksource.WriteRunStatus(path, want))

	got, err := disksource.ReadRunStatus(path)
	require.NoError(t, err)
	require.Equal(t, want, got)
	require.Equal(t, filepath.Dir(statePath), filepath.Dir(path))
}

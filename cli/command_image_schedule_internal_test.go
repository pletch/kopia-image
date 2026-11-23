package cli

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/repo/compression"
	"github.com/kopia/kopia/snapshot/image/disksource"
	"github.com/kopia/kopia/snapshot/image/schedule"
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

	schedules, err := c.schedules()
	require.NoError(t, err)

	creates, check, err := c.tasks(schedules)
	require.NoError(t, err)
	require.Len(t, creates, 1)

	create := creates[0]

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
		"--wait-for-other-run",
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
	_, check, err = c.tasks(schedules)
	require.NoError(t, err)
	require.Equal(t, []string{"image", "check", "--sample", "2.5", "--ntfs"}, check.Arguments[len(check.Arguments)-5:])

	c.checkNTFS = false

	c.checkDay = "none"
	_, check, err = c.tasks(schedules)
	require.NoError(t, err)
	require.Nil(t, check)

	c.at = "25:00"
	_, err = c.schedules()
	require.ErrorContains(t, err, "invalid time of day")
}

func TestImageScheduleInstall_diskSchedules(t *testing.T) {
	c := &commandImageScheduleInstall{
		volumes:       []string{"C:"},
		at:            "02:00",
		diskSchedules: []string{"D:,E:=21:30/Mon,Thu", "F:=02:00", "G:=21:30/thursday,monday"},
		flushInterval: time.Minute,
		loc: imageScheduleLocation{
			taskFolder: `\Kopia`,
			configDir:  `C:\ProgramData\kopia\image`,
			statePath:  `C:\ProgramData\kopia\image-shadow-copies.json`,
		},
	}

	schedules, err := c.schedules()
	require.NoError(t, err)

	// F: is at the time of the volumes given as arguments, and G: on the
	// same days as D: and E:: each pair is one schedule
	require.Equal(t, []schedule.DiskSchedule{
		{Volumes: []string{"C:", "F:"}, At: "02:00", Task: "Image create"},
		{Volumes: []string{"D:", "E:", "G:"}, At: "21:30", Days: []string{"Monday", "Thursday"}, Task: "Image create 2"},
	}, schedules)
	require.Equal(t, []string{"C:", "F:", "D:", "E:", "G:"}, allScheduledVolumes(schedules))

	creates, _, err := c.tasks(schedules)
	require.NoError(t, err)
	require.Len(t, creates, 2)
	require.Equal(t, `\Kopia\Image create 2`, creates[1].Name)
	require.Equal(t, 21, creates[1].At.Hour())
	require.Equal(t, []time.Weekday{time.Monday, time.Thursday}, creates[1].Days)
	require.Contains(t, creates[1].Arguments, "D:")
	require.NotContains(t, creates[1].Arguments, "C:")

	// a volume mounted in a folder is kept as given, even with "=" in it
	_, at, _, err := parseDiskSchedule(`C:\Mount\a=b=21:00/Mon`)
	require.NoError(t, err)
	require.Equal(t, "21:00", at)

	c.volumes = []string{`C:\Mount\Data`}
	c.diskSchedules = []string{`c:\mount\data=03:00`}
	_, err = c.schedules()
	require.ErrorContains(t, err, "more than one schedule")

	c.diskSchedules = []string{`C:\Mount\Other=03:00`}
	schedules, err = c.schedules()
	require.NoError(t, err)
	require.Equal(t, []string{`C:\Mount\Data`}, schedules[0].Volumes)

	// only per-disk schedules
	c.volumes = nil
	c.diskSchedules = []string{"D:=03:00"}
	schedules, err = c.schedules()
	require.NoError(t, err)
	require.Equal(t, []schedule.DiskSchedule{{Volumes: []string{"D:"}, At: "03:00", Task: "Image create"}}, schedules)

	for spec, want := range map[string]string{
		"D:":              "expected VOLUMES=HH:MM",
		"=03:00":          "expected VOLUMES=HH:MM",
		"D:=3pm":          "invalid time of day",
		"D:=03:00/Funday": "invalid day",
		"D:,D:=03:00":     "listed twice",
		"D:,d:=03:00":     "listed twice",
	} {
		c.diskSchedules = []string{spec}
		_, err := c.schedules()
		require.ErrorContains(t, err, want, spec)
	}

	c.volumes = []string{"D:"}
	c.diskSchedules = []string{"D:=03:00"}
	_, err = c.schedules()
	require.ErrorContains(t, err, "more than one schedule")

	c.volumes = nil
	c.diskSchedules = nil
	_, err = c.schedules()
	require.ErrorContains(t, err, "give the volumes to image")
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

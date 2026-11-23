package schedule_test

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/snapshot/image/schedule"
)

func TestSettings_schedules(t *testing.T) {
	// settings written before there was a schedule for each disk
	old := &schedule.Settings{Volumes: []string{"C:", "D:"}, At: "02:00", Days: []string{"Monday"}}
	require.Equal(t, []schedule.DiskSchedule{
		{Volumes: []string{"C:", "D:"}, At: "02:00", Days: []string{"Monday"}, Task: "Image create"},
	}, old.Schedules())
	require.Equal(t, []string{"Image create"}, old.CreateTaskNames())

	s := &schedule.Settings{
		Volumes: []string{"C:", "D:"},
		At:      "02:00",
		Disks: []schedule.DiskSchedule{
			{Volumes: []string{"C:"}, At: "02:00", Task: schedule.CreateTaskNameFor(0)},
			{Volumes: []string{"D:"}, At: "21:00", Task: schedule.CreateTaskNameFor(1)},
		},
	}
	require.Equal(t, s.Disks, s.Schedules())
	require.Equal(t, []string{"Image create", "Image create 2"}, s.CreateTaskNames())

	// no settings: the single task kopia has always had
	var none *schedule.Settings
	require.Equal(t, []string{"Image create"}, none.CreateTaskNames())
	require.Nil(t, (&schedule.Settings{}).Schedules())
}

func TestSettings_roundTrip(t *testing.T) {
	path := schedule.SettingsPath(t.TempDir())
	want := &schedule.Settings{
		Volumes: []string{"C:", "D:"},
		At:      "02:00",
		Disks: []schedule.DiskSchedule{
			{Volumes: []string{"C:"}, At: "02:00", Task: "Image create"},
			{Volumes: []string{"D:"}, At: "21:00", Days: []string{"Sunday"}, Task: "Image create 2"},
		},
		CheckDay: "none",
	}

	require.NoError(t, schedule.WriteSettings(path, want))

	got, err := schedule.ReadSettings(path)
	require.NoError(t, err)
	require.Equal(t, want, got)
	require.Equal(t, "image-schedule.json", filepath.Base(path))
}

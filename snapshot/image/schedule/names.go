package schedule

import (
	"path/filepath"
	"strings"
)

// Names of the scheduled image tasks that "kopia image schedule install"
// registers.
const (
	DefaultTaskFolder = `\Kopia`
	CreateTaskName    = "Image create"
	CheckTaskName     = "Image check"
)

// TaskPath returns the full name of a task in a Task Scheduler folder, e.g.
// `\Kopia\Image create`.
func TaskPath(folder, name string) string {
	return strings.TrimSuffix(folder, `\`) + `\` + name
}

// StateDir returns ProgramData\kopia, which every user can read: kopia
// image keeps there what the web UI shows, such as the last run, the
// schedule settings and the last recovery media built. Only SYSTEM and
// Administrators can write it.
func StateDir() (string, error) {
	pd, err := ProgramDataDir()
	if err != nil {
		return "", err
	}

	return filepath.Join(pd, "kopia"), nil
}

// DefaultConfigDir returns the folder under ProgramData that holds the
// scheduled tasks' copy of kopia and repository connection, with its
// password. Only SYSTEM and Administrators can open it.
func DefaultConfigDir() (string, error) {
	dir, err := StateDir()
	if err != nil {
		return "", err
	}

	return filepath.Join(dir, "image"), nil
}

// TaskStatus is the state of a scheduled task as Task Scheduler reports it.
// Times and results are as schtasks formats them in the computer's locale.
type TaskStatus struct {
	Status      string `json:"status"`
	NextRunTime string `json:"nextRunTime"`
	LastRunTime string `json:"lastRunTime"`
	LastResult  string `json:"lastResult"`
}

// StatusFromProperties picks the task status out of the properties Query
// returns.
func StatusFromProperties(props map[string]string) TaskStatus {
	return TaskStatus{
		Status:      props["Status"],
		NextRunTime: props["Next Run Time"],
		LastRunTime: props["Last Run Time"],
		LastResult:  props["Last Result"],
	}
}

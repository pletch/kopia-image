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

// DefaultConfigDir returns the folder under ProgramData that holds the
// scheduled tasks' repository connection and the record of the last
// recovery media built.
func DefaultConfigDir() (string, error) {
	pd, err := ProgramDataDir()
	if err != nil {
		return "", err
	}

	return filepath.Join(pd, "kopia", "image"), nil
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

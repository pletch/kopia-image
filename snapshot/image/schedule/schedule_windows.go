package schedule

import (
	"bufio"
	"bytes"
	"context"
	stderrors "errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/pkg/errors"
	"golang.org/x/sys/windows"

	"github.com/kopia/kopia/internal/clock"
)

// Install registers a task, replacing a task of the same name.
func Install(ctx context.Context, t *Task) error {
	data, err := t.XML(clock.Now())
	if err != nil {
		return err
	}

	f, err := os.CreateTemp("", "kopia-task-*.xml")
	if err != nil {
		return errors.Wrap(err, "unable to write the task definition")
	}

	defer os.Remove(f.Name()) //nolint:errcheck

	_, writeErr := f.Write(data)
	if err := stderrors.Join(writeErr, f.Close()); err != nil {
		return errors.Wrap(err, "unable to write the task definition")
	}

	_, err = schtasks(ctx, "/Create", "/TN", t.Name, "/XML", f.Name(), "/F")

	return err
}

// Delete removes a task. It returns ErrTaskNotFound if there is none.
func Delete(ctx context.Context, name string) error {
	if _, err := Query(ctx, name); err != nil {
		return err
	}

	_, err := schtasks(ctx, "/Delete", "/TN", name, "/F")

	return err
}

// Run starts a task now.
func Run(ctx context.Context, name string) error {
	_, err := schtasks(ctx, "/Run", "/TN", name)
	return err
}

// Query returns a task's properties as schtasks /V lists them, such as
// "Next Run Time", "Last Run Time" and "Last Result". It returns
// ErrTaskNotFound if there is no such task.
func Query(ctx context.Context, name string) (map[string]string, error) {
	out, err := schtasks(ctx, "/Query", "/TN", name, "/FO", "LIST", "/V")
	if err != nil {
		if strings.Contains(err.Error(), "cannot find") {
			return nil, errors.Wrap(ErrTaskNotFound, name)
		}

		return nil, err
	}

	props := map[string]string{}

	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ":")
		if ok {
			if _, seen := props[strings.TrimSpace(k)]; !seen {
				props[strings.TrimSpace(k)] = strings.TrimSpace(v)
			}
		}
	}

	return props, nil
}

// schtasks runs the Task Scheduler tool from the Windows system folder: the
// caller is an administrator, so a program of that name found elsewhere on
// PATH must not run.
func schtasks(ctx context.Context, args ...string) ([]byte, error) {
	system32, err := windows.GetSystemDirectory()
	if err != nil {
		return nil, errors.Wrap(err, "unable to find the Windows system folder")
	}

	out, err := exec.CommandContext(ctx, filepath.Join(system32, "schtasks.exe"), args...).CombinedOutput() //nolint:gosec
	if err != nil {
		err = errors.Errorf("schtasks %v failed: %v: %s", args[0], err, bytes.TrimSpace(out))

		if isAccessDenied(out) {
			return out, errors.Wrap(ErrAccessDenied, err.Error())
		}

		return out, err
	}

	return out, nil
}

// isAccessDenied reports whether schtasks output says access is denied.
// schtasks prints the system message of ERROR_ACCESS_DENIED, in the
// language of the computer.
func isAccessDenied(out []byte) bool {
	msg := strings.TrimSuffix(windows.ERROR_ACCESS_DENIED.Error(), ".")

	return bytes.Contains(out, []byte(msg)) || bytes.Contains(out, []byte("Access is denied"))
}

// ProgramDataDir returns the ProgramData folder.
func ProgramDataDir() (string, error) {
	dir, err := windows.KnownFolderPath(windows.FOLDERID_ProgramData, 0)
	return dir, errors.Wrap(err, "unable to find ProgramData")
}

// SetEnabled enables or disables a task: a disabled task does not start on
// its schedule, nor retry a failed run. It returns ErrTaskNotFound if there
// is no such task.
func SetEnabled(ctx context.Context, name string, enabled bool) error {
	if _, err := Query(ctx, name); err != nil {
		return err
	}

	flag := "/DISABLE"
	if enabled {
		flag = "/ENABLE"
	}

	_, err := schtasks(ctx, "/Change", "/TN", name, flag)

	return err
}

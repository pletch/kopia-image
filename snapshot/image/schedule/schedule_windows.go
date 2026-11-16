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

// restrictedDACL grants full control to SYSTEM and Administrators only, is
// protected from inherited entries, and is inherited by everything created
// below the folder.
const restrictedDACL = "D:PAI(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)"

// ErrTaskNotFound is returned when a task does not exist.
var ErrTaskNotFound = errors.New("scheduled task not found")

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

func schtasks(ctx context.Context, args ...string) ([]byte, error) {
	out, err := exec.CommandContext(ctx, "schtasks.exe", args...).CombinedOutput() //nolint:gosec
	if err != nil {
		return out, errors.Errorf("schtasks %v failed: %v: %s", args[0], err, bytes.TrimSpace(out))
	}

	return out, nil
}

// SecureDirectory creates a folder, if needed, that only SYSTEM and
// Administrators can open, for the scheduled task's repository config and
// password.
func SecureDirectory(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil { //nolint:mnd
		return errors.Wrapf(err, "unable to create %v", path)
	}

	sd, err := windows.SecurityDescriptorFromString(restrictedDACL)
	if err != nil {
		return errors.Wrap(err, "invalid security descriptor")
	}

	dacl, _, err := sd.DACL()
	if err != nil {
		return errors.Wrap(err, "invalid security descriptor")
	}

	err = windows.SetNamedSecurityInfo(filepath.Clean(path), windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil)

	return errors.Wrapf(err, "unable to restrict access to %v", path)
}

// DirectoryDACL returns the DACL of a folder in SDDL form.
func DirectoryDACL(path string) (string, error) {
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return "", errors.Wrapf(err, "unable to read the permissions of %v", path)
	}

	return sd.String(), nil
}

// ProgramDataDir returns the ProgramData folder.
func ProgramDataDir() (string, error) {
	dir, err := windows.KnownFolderPath(windows.FOLDERID_ProgramData, 0)
	return dir, errors.Wrap(err, "unable to find ProgramData")
}

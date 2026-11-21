package schedule

import (
	"context"
	"errors"
	"runtime"
	"strings"

	"github.com/go-ole/go-ole"
	"github.com/go-ole/go-ole/oleutil"
	pkgerrors "github.com/pkg/errors"
)

// ShareStatus gives an existing task the security descriptor of new tasks,
// so that every user can read its status. Registering a task again with
// schtasks /Create /F keeps the permissions it had, so tasks scheduled before
// kopia set them stay hidden from KopiaUI otherwise. It returns
// ErrTaskNotFound if there is no such task.
func ShareStatus(ctx context.Context, name string) error {
	if _, err := Query(ctx, name); err != nil {
		return err
	}

	return setTaskSecurityDescriptor(name, taskSecurityDescriptor)
}

// setTaskSecurityDescriptor replaces the permissions of a task with sddl.
func setTaskSecurityDescriptor(name, sddl string) error {
	return withTask(name, func(task *ole.IDispatch) error {
		if _, err := oleutil.CallMethod(task, "SetSecurityDescriptor", sddl, 0); err != nil {
			return pkgerrors.Wrapf(err, "unable to set the permissions of the task %v", name)
		}

		return nil
	})
}

// getTaskSecurityDescriptor returns the discretionary access control list of
// a task as SDDL.
func getTaskSecurityDescriptor(name string) (string, error) {
	var sddl string

	err := withTask(name, func(task *ole.IDispatch) error {
		const daclSecurityInformation = 4

		v, err := oleutil.CallMethod(task, "GetSecurityDescriptor", daclSecurityInformation)
		if err != nil {
			return pkgerrors.Wrapf(err, "unable to read the permissions of the task %v", name)
		}

		sddl = v.ToString()

		return nil
	})

	return sddl, err
}

// withTask calls f with a registered task of Task Scheduler.
func withTask(name string, f func(task *ole.IDispatch) error) error {
	return withCOM(func() error {
		unk, err := oleutil.CreateObject("Schedule.Service")
		if err != nil {
			return pkgerrors.Wrap(err, "unable to open Task Scheduler")
		}
		defer unk.Release()

		svc, err := unk.QueryInterface(ole.IID_IDispatch)
		if err != nil {
			return pkgerrors.Wrap(err, "unable to open Task Scheduler")
		}
		defer svc.Release()

		if _, err := oleutil.CallMethod(svc, "Connect"); err != nil {
			return pkgerrors.Wrap(err, "unable to connect to Task Scheduler")
		}

		folderPath, taskName := `\`, strings.TrimPrefix(name, `\`)
		if i := strings.LastIndex(name, `\`); i > 0 {
			folderPath, taskName = name[:i], name[i+1:]
		}

		fv, err := oleutil.CallMethod(svc, "GetFolder", folderPath)
		if err != nil {
			return pkgerrors.Wrapf(err, "unable to open the task folder %v", folderPath)
		}

		folder := fv.ToIDispatch()
		defer folder.Release()

		tv, err := oleutil.CallMethod(folder, "GetTask", taskName)
		if err != nil {
			return pkgerrors.Wrapf(err, "unable to open the task %v", name)
		}

		task := tv.ToIDispatch()
		defer task.Release()

		return f(task)
	})
}

// withCOM runs f on a thread with COM initialized: COM objects belong to the
// thread that created them.
func withCOM(f func() error) error {
	runtime.LockOSThread()

	defer runtime.UnlockOSThread()

	if err := ole.CoInitializeEx(0, ole.COINIT_MULTITHREADED); err != nil {
		var oerr *ole.OleError
		// S_FALSE: already initialized on this thread
		if !errors.As(err, &oerr) || oerr.Code() != 1 {
			return pkgerrors.Wrap(err, "unable to initialize COM")
		}
	}

	defer ole.CoUninitialize()

	return f()
}

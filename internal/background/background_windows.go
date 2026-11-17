package background

import (
	"github.com/pkg/errors"
	"golang.org/x/sys/windows"
)

func enter() (func(), error) {
	if err := windows.SetPriorityClass(windows.CurrentProcess(), windows.PROCESS_MODE_BACKGROUND_BEGIN); err != nil {
		return func() {}, errors.Wrap(err, "unable to enter background mode")
	}

	return func() {
		windows.SetPriorityClass(windows.CurrentProcess(), windows.PROCESS_MODE_BACKGROUND_END) //nolint:errcheck
	}, nil
}

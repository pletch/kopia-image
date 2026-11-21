package background

import (
	"bytes"
	"os"
	"strconv"
	"strings"

	"github.com/pkg/errors"
	"golang.org/x/sys/unix"
)

// Background priorities on Linux: a nice value that yields the processors to
// interactive programs and services, and the lowest priority of the normal
// disk class, rather than the idle class, under which a busy disk would stall
// kopia altogether.
const (
	backgroundNice = 10

	ioprioClassShift  = 13
	ioprioClassBE     = 2
	ioprioClassIdle   = 3
	ioprioLowestBE    = 7
	ioprioWhoProcess  = 1
	ioprioLevelMask   = 1<<ioprioClassShift - 1
	threadListPasses  = 3
	statNiceField     = 16 // after the command name: state is field 0
	minStatFieldCount = statNiceField + 1
)

// enter lowers the priority of every thread of the process. Linux keeps
// priorities per thread, and a new thread starts with the priority of the
// thread that creates it, so once every thread is lowered the threads the Go
// runtime starts later are too. Threads already at a lower priority, as set
// by systemd's Nice= or IOSchedulingClass=, keep theirs.
//
// Raising the priority again needs privileges that kopia does not have, so
// leave does nothing: background mode lasts as long as the process.
func enter() (func(), error) {
	seen := map[int]bool{}

	var errs []error

	// threads started while the list is walked are walked in the next pass
	for range threadListPasses {
		tids, err := threadIDs()
		if err != nil {
			return func() {}, err
		}

		added := false

		for _, tid := range tids {
			if seen[tid] {
				continue
			}

			seen[tid] = true
			added = true

			if err := lowerThread(tid); err != nil {
				errs = append(errs, err)
			}
		}

		if !added {
			break
		}
	}

	if len(errs) > 0 {
		return func() {}, errors.Errorf("unable to lower the priority of every thread: %v", errs[0])
	}

	return func() {}, nil
}

func threadIDs() ([]int, error) {
	entries, err := os.ReadDir("/proc/self/task")
	if err != nil {
		return nil, errors.Wrap(err, "unable to list the threads")
	}

	var out []int

	for _, e := range entries {
		if tid, err := strconv.Atoi(e.Name()); err == nil {
			out = append(out, tid)
		}
	}

	return out, nil
}

func lowerThread(tid int) error {
	nice, err := threadNice(tid)
	if err != nil {
		return err
	}

	if nice < backgroundNice {
		if err := unix.Setpriority(unix.PRIO_PROCESS, tid, backgroundNice); err != nil {
			return errors.Wrapf(err, "thread %v", tid)
		}
	}

	return lowerThreadIO(tid)
}

// lowerThreadIO gives a thread the lowest priority of the normal disk class,
// unless it is in the idle class or already there.
func lowerThreadIO(tid int) error {
	cur, _, errno := unix.Syscall(unix.SYS_IOPRIO_GET, ioprioWhoProcess, uintptr(tid), 0)
	if errno != 0 {
		return errors.Wrapf(errno, "thread %v", tid)
	}

	class, level := int(cur>>ioprioClassShift), int(cur&ioprioLevelMask)

	if class == ioprioClassIdle || (class == ioprioClassBE && level >= ioprioLowestBE) {
		return nil
	}

	// a thread without a class has the priority its nice value implies,
	// which is above the lowest
	if _, _, errno := unix.Syscall(unix.SYS_IOPRIO_SET, ioprioWhoProcess, uintptr(tid), ioprioClassBE<<ioprioClassShift|ioprioLowestBE); errno != 0 {
		return errors.Wrapf(errno, "thread %v", tid)
	}

	return nil
}

// threadNice reads a thread's nice value from /proc, which, unlike
// getpriority, reports it as is.
func threadNice(tid int) (int, error) {
	b, err := os.ReadFile("/proc/self/task/" + strconv.Itoa(tid) + "/stat")
	if err != nil {
		return 0, errors.Wrapf(err, "thread %v", tid)
	}

	// the command name, in parentheses, can hold spaces
	_, rest, ok := bytes.CutLast(b, []byte(")"))
	if !ok {
		return 0, errors.Errorf("thread %v: invalid stat", tid)
	}

	fields := strings.Fields(string(rest))
	if len(fields) < minStatFieldCount {
		return 0, errors.Errorf("thread %v: invalid stat", tid)
	}

	nice, err := strconv.Atoi(fields[statNiceField])

	return nice, errors.Wrapf(err, "thread %v: invalid nice value", tid)
}

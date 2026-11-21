package background

import (
	"runtime"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// TestEnter lowers the priority of the test process, which it cannot raise
// again, and checks every thread, including those started afterwards.
func TestEnter(t *testing.T) {
	leave, err := Enter()
	require.NoError(t, err)

	defer leave()

	// threads the runtime starts later inherit the priority
	var wg sync.WaitGroup

	for range 4 {
		wg.Go(func() {
			runtime.LockOSThread()

			defer runtime.UnlockOSThread()

			nice, err := threadNice(unix.Gettid())
			require.NoError(t, err)
			require.GreaterOrEqual(t, nice, backgroundNice)
		})
	}

	wg.Wait()

	tids, err := threadIDs()
	require.NoError(t, err)
	require.NotEmpty(t, tids)

	for _, tid := range tids {
		nice, err := threadNice(tid)
		require.NoError(t, err)
		require.GreaterOrEqual(t, nice, backgroundNice, "thread %v", tid)

		v, _, errno := unix.Syscall(unix.SYS_IOPRIO_GET, ioprioWhoProcess, uintptr(tid), 0)
		require.Zero(t, errno)

		class, level := int(v>>ioprioClassShift), int(v&ioprioLevelMask)
		require.True(t, class == ioprioClassIdle || (class == ioprioClassBE && level == ioprioLowestBE), "thread %v: class %v level %v", tid, class, level)
	}

	// entering again keeps the priority, never raises it
	_, err = Enter()
	require.NoError(t, err)
}

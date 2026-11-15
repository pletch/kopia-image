package wakeevents

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// newTestManager returns a manager that checks often but only reports a gap of more than
// a second, so late timer ticks on a busy machine never look like a wake.
func newTestManager(t *testing.T) *windowsManager {
	t.Helper()

	mgr, ok := newPlatformManager().(*windowsManager)
	require.True(t, ok)

	mgr.checkInterval = 10 * time.Millisecond
	mgr.gapThreshold = time.Second

	return mgr
}

// simulateSleep moves the last check back as if the system had been asleep for d.
func (m *windowsManager) simulateSleep(d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.lastCheck = m.lastCheck.Add(-d)
}

// requireCallCount waits for count to reach want and then checks that it stays there.
func requireCallCount(t *testing.T, count *atomic.Int32, want int32) {
	t.Helper()

	require.Eventually(t, func() bool { return count.Load() >= want }, 5*time.Second, 5*time.Millisecond)

	// give a second, spurious detection the chance to show up
	time.Sleep(100 * time.Millisecond)
	require.Equal(t, want, count.Load())
}

func TestWakeDetection(t *testing.T) {
	mgr := newTestManager(t)

	var count atomic.Int32

	mgr.OnWake(func() { count.Add(1) })
	mgr.Start(t.Context())

	time.Sleep(50 * time.Millisecond)
	require.Zero(t, count.Load(), "no wake must be reported without a gap")

	mgr.simulateSleep(5 * time.Second)

	requireCallCount(t, &count, 1)
}

func TestMultipleCallbacks(t *testing.T) {
	mgr := newTestManager(t)

	var count atomic.Int32

	mgr.OnWake(func() { count.Add(1) })
	mgr.OnWake(func() { count.Add(1) })
	mgr.Start(t.Context())

	mgr.simulateSleep(5 * time.Second)

	requireCallCount(t, &count, 2)
}

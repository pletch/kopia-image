package cli

import (
	"context"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/pkg/errors"
	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/snapshot/image/disksource"
)

func TestImageCreateWillRetry(t *testing.T) {
	// as the laptop reported it when its network went away mid-run
	lost := errors.Wrap(errors.Wrap(errors.Wrap(io.EOF, "EOF"), "error waiting for async writes"), "unable to image disk 0")
	unreachable := errors.Wrap(repo.ErrServerUnreachable, "unable to establish session")

	c := &commandImageCreate{retriesLeft: 2}

	require.True(t, c.willRetry(lost))
	require.True(t, c.willRetry(unreachable))

	// failures that another attempt would not cure
	require.False(t, c.willRetry(errors.New("access denied")))
	require.False(t, c.willRetry(errors.Wrap(errImageStopped, "stopped")))
	require.False(t, c.willRetry(errors.Wrap(disksource.ErrAlreadyRunning, "busy")))
	require.False(t, c.willRetry(nil))

	// the last attempt records its failure
	c.retriesLeft = 0
	require.False(t, c.willRetry(lost))

	require.Len(t, imageConnectionRetryDelays, 3)
}

func TestImageCreateWaitForRunToEnd(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")

	saved := imageRunWaitInterval
	imageRunWaitInterval = 10 * time.Millisecond

	t.Cleanup(func() { imageRunWaitInterval = saved })

	c := &commandImageCreate{statePath: statePath}

	// nothing running: no wait, and the start stays as it was
	start := time.Date(2026, 11, 23, 2, 0, 0, 0, time.UTC)
	c.runStart = start
	require.NoError(t, c.waitForRunToEnd(t.Context()))
	require.Equal(t, start, c.runStart)

	other, err := disksource.OpenShadowState(statePath)
	require.NoError(t, err)

	running, err := disksource.IsRunning(statePath)
	require.NoError(t, err)
	require.True(t, running)

	done := make(chan error, 1)

	go func() { done <- c.waitForRunToEnd(t.Context()) }()

	select {
	case err := <-done:
		t.Fatalf("ended while the other run holds the lock: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	require.NoError(t, other.Close())
	require.NoError(t, <-done)

	// the run starts when the other ended, not when it began to wait
	require.True(t, c.runStart.After(start))

	// the lock was only tested: a run can take it now
	again, err := disksource.OpenShadowState(statePath)
	require.NoError(t, err)
	require.NoError(t, again.Close())
}

func TestImageCreateWaitForRunToEnd_canceled(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")

	other, err := disksource.OpenShadowState(statePath)
	require.NoError(t, err)

	t.Cleanup(func() { other.Close() })

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()

	c := &commandImageCreate{statePath: statePath}
	require.ErrorIs(t, c.waitForRunToEnd(ctx), context.DeadlineExceeded)
}

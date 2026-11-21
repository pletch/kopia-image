package cli

import (
	"io"
	"testing"

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

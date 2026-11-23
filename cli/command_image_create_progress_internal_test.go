package cli

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/internal/clock"
	"github.com/kopia/kopia/internal/testlogging"
	"github.com/kopia/kopia/snapshot/image"
	"github.com/kopia/kopia/snapshot/image/disksource"
)

func TestImageRunMonitor(t *testing.T) {
	ctx := testlogging.Context(t)
	dir := t.TempDir()
	start := clock.Now()

	// a request left for an earlier run is removed, not obeyed
	require.NoError(t, disksource.WriteStopRequest(disksource.StopPathIn(dir), &disksource.StopRequest{RequestedAt: start.Add(-time.Hour)}))

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var progress image.Progress

	m := startImageRunMonitor(ctx, dir, "digest", start, []string{"C:"}, 2, &progress, func() int64 { return 1234 }, cancel)
	m.setDisk(1, 3)

	require.Eventually(t, func() bool {
		p, err := disksource.ReadRunProgress(disksource.ProgressPathIn(dir))
		return err == nil && p.Disk == 1 && p.DiskNumber == 3 && p.Disks == 2 && p.UploadedBytes == 1234
	}, 10*time.Second, 50*time.Millisecond)

	require.NoFileExists(t, disksource.StopPathIn(dir))
	require.False(t, m.stopped())
	require.NoError(t, runCtx.Err())

	// a stop request newer than the run cancels it
	require.NoError(t, disksource.WriteStopRequest(disksource.StopPathIn(dir), &disksource.StopRequest{RequestedAt: clock.Now()}))

	require.Eventually(t, m.stopped, 10*time.Second, 50*time.Millisecond)
	require.Eventually(t, func() bool { return runCtx.Err() != nil }, 10*time.Second, 50*time.Millisecond)

	p, err := disksource.ReadRunProgress(disksource.ProgressPathIn(dir))
	require.NoError(t, err)
	require.True(t, p.Stopping || m.stopped())

	m.close(ctx)

	require.NoFileExists(t, disksource.ProgressPathIn(dir))
	require.NoFileExists(t, disksource.StopPathIn(dir))
}

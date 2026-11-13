package server

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/internal/clock"
	"github.com/kopia/kopia/internal/repotesting"
	"github.com/kopia/kopia/internal/testutil"
	"github.com/kopia/kopia/internal/uitask"
	"github.com/kopia/kopia/notification/notifydata"
	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/snapshot"
	"github.com/kopia/kopia/snapshot/policy"
	"github.com/kopia/kopia/snapshot/upload"
)

type blockingNewWriterRepository struct {
	repo.Repository
}

func (r *blockingNewWriterRepository) NewWriter(ctx context.Context, _ repo.WriteSessionOptions) (context.Context, repo.RepositoryWriter, error) {
	<-ctx.Done()
	return nil, nil, ctx.Err()
}

type immediateCancelController struct {
	taskID string
}

func (c *immediateCancelController) CurrentTaskID() string {
	return c.taskID
}

func (c *immediateCancelController) OnCancel(cancelFunc context.CancelFunc) {
	cancelFunc()
}

func (c *immediateCancelController) ReportCounters(map[string]uitask.CounterValue) {}

func (c *immediateCancelController) ReportProgressInfo(string) {}

func TestSourceManagerSnapshotInternalCancelBeforeWriterCreation(t *testing.T) {
	t.Parallel()

	ctx, env := repotesting.NewEnvironment(t, repotesting.FormatNotImportant)

	s := &sourceManager{
		src: snapshot.SourceInfo{
			Host:     "host",
			UserName: "user",
			Path:     testutil.TempDirectory(t),
		},
		rep:      &blockingNewWriterRepository{Repository: env.Repository},
		closed:   make(chan struct{}),
		progress: &upload.CountingUploadProgress{},
	}

	ctrl := &immediateCancelController{taskID: "task-1"}
	result := &notifydata.ManifestWithError{}

	done := make(chan error, 1)

	go func() {
		done <- s.snapshotInternal(ctx, ctrl, result)
	}()

	select {
	case err := <-done:
		require.Error(t, err)
		require.ErrorIs(t, err, context.Canceled)
		require.ErrorContains(t, err, "unable to create writer")

	case <-time.After(2 * time.Second):
		t.Fatal("snapshotInternal did not return after task cancellation")
	}
}

func TestSourceManagerBackoffAfterFailedScheduledSnapshot(t *testing.T) {
	sm := &sourceManager{
		pol: policy.SchedulingPolicy{
			IntervalSeconds: int64(time.Hour.Seconds()),
		},
		snapshotRequests: make(chan struct{}, 1),
	}

	initialSnapshotTime := clock.Now().Add(time.Minute)
	sm.nextSnapshotTime = &initialSnapshotTime
	sm.scheduleSnapshotNow()

	_, ok := sm.getNextSnapshotTime()
	require.False(t, ok, "queued snapshot should clear the next snapshot time")

	beforeBackoff := clock.Now()

	sm.backoffBeforeNextSnapshot()

	nextSnapshotTime, ok := sm.getNextSnapshotTime()
	require.True(t, ok, "failed scheduled snapshot should be retried")
	require.WithinDuration(t, beforeBackoff.Add(failedSnapshotRetryInterval), nextSnapshotTime, time.Second)
}

func TestSourceManagerBackoffAfterFailedManualSnapshot(t *testing.T) {
	sm := &sourceManager{
		pol: policy.SchedulingPolicy{
			Manual: true,
		},
		snapshotRequests: make(chan struct{}, 1),
	}

	initialSnapshotTime := clock.Now().Add(time.Minute)
	sm.nextSnapshotTime = &initialSnapshotTime
	sm.scheduleSnapshotNow()
	sm.backoffBeforeNextSnapshot()

	_, ok := sm.getNextSnapshotTime()
	require.False(t, ok, "manual snapshot should not be retried")
}

type blockingSnapshotServer struct {
	entered  chan struct{}
	release  chan struct{}
	runCount atomic.Int32
}

func (s *blockingSnapshotServer) runSnapshotTask(
	_ context.Context,
	_ snapshot.SourceInfo,
	_ func(ctx context.Context, ctrl uitask.Controller, result *notifydata.ManifestWithError) error,
) error {
	s.runCount.Add(1)

	select {
	case s.entered <- struct{}{}:
	default:
	}

	<-s.release

	return nil
}

func (s *blockingSnapshotServer) refreshScheduler(_ string) {}

// TestSourceManager_refreshStatus_whileSnapshotPending verifies that refreshing a
// source's status while its snapshot is queued (waiting for an upload slot) does not
// compute an overdue next snapshot time, which would make the scheduler queue the same
// snapshot a second time and run it back-to-back.

func TestSourceManager_refreshStatus_whileSnapshotPending(t *testing.T) {
	ctx, env := repotesting.NewEnvironment(t, repotesting.FormatNotImportant)

	si := env.LocalPathSourceInfo(testutil.TempDirectory(t))

	require.NoError(t, policy.SetPolicy(ctx, env.RepositoryWriter, si, &policy.Policy{
		SchedulingPolicy: policy.SchedulingPolicy{
			IntervalSeconds: int64(time.Hour / time.Second),
		},
	}))
	require.NoError(t, env.RepositoryWriter.Flush(ctx))

	srv := &blockingSnapshotServer{
		entered: make(chan struct{}, 1),
		release: make(chan struct{}),
	}

	sm := newSourceManager(si, srv, env.RepositoryWriter)
	sm.start(ctx, true)

	// The scheduler fires: the snapshot is queued and blocks waiting for an upload slot.
	sm.scheduleSnapshotNow()

	select {
	case <-srv.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("snapshot task was not started")
	}

	// The periodic source refresh (Server.syncSourcesLocked) runs while the snapshot is
	// still pending.
	sm.refreshStatus(ctx)

	if nst, ok := sm.getNextSnapshotTime(); ok {
		require.True(t, nst.After(clock.Now()),
			"pending source was rescheduled as overdue (next snapshot time %v)", nst)
	}

	close(srv.release)
	sm.stop(ctx)
	sm.waitUntilStopped()

	require.Equal(t, int32(1), srv.runCount.Load())
}

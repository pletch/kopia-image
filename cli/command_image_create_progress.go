package cli

import (
	"context"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pkg/errors"

	"github.com/kopia/kopia/internal/clock"
	"github.com/kopia/kopia/snapshot/image"
	"github.com/kopia/kopia/snapshot/image/disksource"
)

// errImageStopped ends an image run that "kopia image stop" asked to stop.
var errImageStopped = errors.New("stopped by request")

// imageRunMonitor writes the progress of an image run to the state folder
// every ProgressInterval, for the web UI, and stops the run when a stop
// request newer than the run appears there.
type imageRunMonitor struct {
	progressPath string
	stopPath     string
	start        time.Time
	volumes      []string
	disks        int

	progress *image.Progress
	uploaded func() int64
	cancel   context.CancelFunc

	// repository identifies the repository the run writes to
	repository string

	mu sync.Mutex
	// +checklocks:mu
	disk int
	// +checklocks:mu
	diskNumber uint32

	stopping atomic.Bool
	done     chan struct{}
	wg       sync.WaitGroup
}

func startImageRunMonitor(ctx context.Context, stateDir, repository string, start time.Time, volumes []string, disks int, progress *image.Progress, uploaded func() int64, cancel context.CancelFunc) *imageRunMonitor {
	m := &imageRunMonitor{
		repository:   repository,
		progressPath: disksource.ProgressPathIn(stateDir),
		stopPath:     disksource.StopPathIn(stateDir),
		start:        start,
		volumes:      volumes,
		disks:        disks,
		progress:     progress,
		uploaded:     uploaded,
		cancel:       cancel,
		done:         make(chan struct{}),
	}

	// a request older than this run was meant for an earlier one
	m.removeStaleStopRequest()

	m.wg.Go(func() {
		t := time.NewTicker(disksource.ProgressInterval)
		defer t.Stop()

		for {
			m.update(ctx)

			select {
			case <-m.done:
				return
			case <-t.C:
			}
		}
	})

	return m
}

// setDisk records which disk is being imaged.
func (m *imageRunMonitor) setDisk(index int, number uint32) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.disk, m.diskNumber = index, number
}

// stopped reports whether the run saw a stop request.
func (m *imageRunMonitor) stopped() bool {
	return m.stopping.Load()
}

// close stops reporting and removes the progress file and stop request.
func (m *imageRunMonitor) close(ctx context.Context) {
	close(m.done)
	m.wg.Wait()

	for _, p := range []string{m.progressPath, m.stopPath} {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			log(ctx).Warnf("unable to remove %v: %v", p, err)
		}
	}
}

func (m *imageRunMonitor) update(ctx context.Context) {
	if r, err := disksource.ReadStopRequest(m.stopPath); err == nil && !r.RequestedAt.Before(m.start) && !m.stopping.Swap(true) {
		log(ctx).Infof("Stopping the image run as requested; what was uploaded is kept.")
		m.cancel()
	}

	m.mu.Lock()
	p := &disksource.RunProgress{
		PID:              os.Getpid(),
		Volumes:          m.volumes,
		StartTime:        m.start,
		UpdatedAt:        clock.Now(),
		Disk:             m.disk,
		Disks:            m.disks,
		DiskNumber:       m.diskNumber,
		ProgressSnapshot: m.progress.Snapshot(),
		UploadedBytes:    m.uploaded(),
		Stopping:         m.stopping.Load(),
		Repository:       m.repository,
	}
	m.mu.Unlock()

	if err := disksource.WriteRunProgress(m.progressPath, p); err != nil {
		log(ctx).Debugf("%v", err)
	}
}

func (m *imageRunMonitor) removeStaleStopRequest() {
	if r, err := disksource.ReadStopRequest(m.stopPath); err == nil && r.RequestedAt.Before(m.start) {
		_ = os.Remove(m.stopPath)
	}
}

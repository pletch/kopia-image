package wakeevents

import (
	"context"
	"sync"
	"time"

	"github.com/kopia/kopia/internal/clock"
	"github.com/kopia/kopia/repo/logging"
)

var log = logging.Module("wakeevents")

// windowsManager detects sleep/wake by monitoring gaps in system time.
// When the elapsed time since the last check exceeds the poll interval + threshold,
// it indicates the system likely woke from sleep.
type windowsManager struct {
	mu        sync.Mutex
	listeners []Listener // +checklocks:mu
	stopChan  chan struct{}
	lastCheck time.Time // +checklocks:mu

	// Gap threshold: if elapsed time > checkInterval + gapThreshold, system woke from sleep
	checkInterval time.Duration
	gapThreshold  time.Duration
}

const (
	defaultCheckInterval = 15 * time.Second
	defaultGapThreshold  = 5 * time.Second
)

func newPlatformManager() Manager {
	return &windowsManager{
		stopChan:      make(chan struct{}),
		checkInterval: defaultCheckInterval,
		gapThreshold:  defaultGapThreshold,
	}
}

// Start begins monitoring for sleep/wake events.
func (m *windowsManager) Start(ctx context.Context) {
	m.mu.Lock()
	m.lastCheck = clock.Now()
	m.mu.Unlock()

	go m.watchForWake(ctx)
}

// OnWake registers a callback to be invoked when the system wakes.
func (m *windowsManager) OnWake(callback Listener) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.listeners = append(m.listeners, callback)
}

// Stop terminates the monitor.
func (m *windowsManager) Stop() {
	close(m.stopChan)
}

// watchForWake monitors for gaps in elapsed time that indicate sleep/wake.
func (m *windowsManager) watchForWake(ctx context.Context) {
	ticker := time.NewTicker(m.checkInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-m.stopChan:
			return
		case <-ticker.C:
			elapsed := m.sinceLastCheck()

			// If the gap is larger than expected, the system likely woke from sleep.
			// Expected gap: checkInterval; threshold allows some clock variance.
			expectedGap := m.checkInterval + m.gapThreshold
			if elapsed > expectedGap {
				log(ctx).Infof("System wake detected (time gap: %v > %v), notifying listeners", elapsed, expectedGap)
				m.notifyListeners(ctx)
			}
		}
	}
}

// sinceLastCheck returns the time since the previous check and records this one.
func (m *windowsManager) sinceLastCheck() time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()

	// clock.Now has no monotonic reading, so the gap is measured on the wall clock,
	// which keeps running while the system sleeps.
	now := clock.Now()
	elapsed := now.Sub(m.lastCheck)
	m.lastCheck = now

	return elapsed
}

// notifyListeners invokes all registered callbacks.
func (m *windowsManager) notifyListeners(ctx context.Context) {
	m.mu.Lock()
	listeners := make([]Listener, len(m.listeners))
	copy(listeners, m.listeners)
	m.mu.Unlock()

	for _, listener := range listeners {
		// Run in a goroutine to avoid blocking if a callback is slow.
		go func(l Listener) {
			defer func() {
				if r := recover(); r != nil {
					log(ctx).Warnf("panic in wake event listener: %v", r)
				}
			}()

			l()
		}(listener)
	}
}

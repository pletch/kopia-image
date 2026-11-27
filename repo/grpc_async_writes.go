package repo

import (
	"context"
	"sync"
)

// asyncWrites tracks content writes in flight on a repository server.
//
// It replaces an errgroup.Group, which can't be waited on while new writes
// start: Flush waited on it while workers kept adding writes, and a
// sync.WaitGroup that goes back up from zero before a waiter wakes panics.
// Wait waits only for writes started before it was called, so a Flush isn't
// held up by writes that start after it, and it honors its context.
type asyncWrites struct {
	mu sync.Mutex

	next     uint64
	inflight map[uint64]struct{}
	err      error

	// changed is closed and replaced whenever a write finishes.
	changed chan struct{}
}

func newAsyncWrites() *asyncWrites {
	return &asyncWrites{
		inflight: map[uint64]struct{}{},
		changed:  make(chan struct{}),
	}
}

// Go runs f in its own goroutine. The first error any write returns is kept
// and returned by every later Wait.
func (a *asyncWrites) Go(f func() error) {
	a.mu.Lock()
	id := a.next
	a.next++
	a.inflight[id] = struct{}{}
	a.mu.Unlock()

	go func() {
		err := f()

		a.mu.Lock()
		defer a.mu.Unlock()

		delete(a.inflight, id)

		if err != nil && a.err == nil {
			a.err = err
		}

		close(a.changed)
		a.changed = make(chan struct{})
	}()
}

// Wait waits until every write started before it was called has finished,
// or ctx is done, and returns the first error any write returned.
func (a *asyncWrites) Wait(ctx context.Context) error {
	a.mu.Lock()
	target := a.next
	a.mu.Unlock()

	for {
		a.mu.Lock()

		pending := false

		for id := range a.inflight {
			if id < target {
				pending = true
				break
			}
		}

		err, changed := a.err, a.changed

		a.mu.Unlock()

		if !pending {
			return err
		}

		select {
		case <-changed:
		case <-ctx.Done():
			return ctx.Err() //nolint:wrapcheck
		}
	}
}

package repo

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestAsyncWritesWaitsForEarlierWritesOnly(t *testing.T) {
	t.Parallel()

	a := newAsyncWrites()

	release1 := make(chan struct{})
	a.Go(func() error { <-release1; return nil })

	waitDone := make(chan error, 1)

	go func() { waitDone <- a.Wait(context.Background()) }()

	// a write started after Wait must not hold it up.
	time.Sleep(10 * time.Millisecond)

	release2 := make(chan struct{})
	defer close(release2)

	a.Go(func() error { <-release2; return nil })

	select {
	case <-waitDone:
		t.Fatal("Wait returned before the earlier write finished")
	case <-time.After(20 * time.Millisecond):
	}

	close(release1)

	select {
	case err := <-waitDone:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Wait waited for a write started after it")
	}
}

func TestAsyncWritesErrorIsSticky(t *testing.T) {
	t.Parallel()

	a := newAsyncWrites()
	boom := errors.New("boom")

	a.Go(func() error { return boom })
	require.ErrorIs(t, a.Wait(context.Background()), boom)

	a.Go(func() error { return nil })
	require.ErrorIs(t, a.Wait(context.Background()), boom)
}

func TestAsyncWritesWaitHonorsContext(t *testing.T) {
	t.Parallel()

	a := newAsyncWrites()

	release := make(chan struct{})
	defer close(release)

	a.Go(func() error { <-release; return nil })

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	require.ErrorIs(t, a.Wait(ctx), context.DeadlineExceeded)
}

// Waiting while writes keep starting from zero must not panic, as
// sync.WaitGroup reuse did.
func TestAsyncWritesConcurrentGoAndWait(t *testing.T) {
	t.Parallel()

	a := newAsyncWrites()

	var wg sync.WaitGroup

	for range 8 {
		wg.Go(func() {
			for range 500 {
				a.Go(func() error { return nil })
			}
		})
	}

	for range 200 {
		require.NoError(t, a.Wait(context.Background()))
	}

	wg.Wait()
	require.NoError(t, a.Wait(context.Background()))
}

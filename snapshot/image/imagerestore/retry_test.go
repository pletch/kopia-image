package imagerestore

import (
	"testing"
	"time"

	"github.com/pkg/errors"
	"github.com/stretchr/testify/require"
)

var (
	errStalled = errors.New("stalled")
	errRefused = errors.New("refused")
)

func TestRetryWrite(t *testing.T) {
	transient := func(err error) bool { return errors.Is(err, errStalled) }
	delays := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}

	cases := []struct {
		name      string
		failures  []error // the errors of the first writes; later writes succeed
		wantErr   error
		wantCalls int
		wantSleep []time.Duration
	}{
		{name: "succeeds", wantCalls: 1},
		{name: "recovers from stalls", failures: []error{errStalled, errStalled}, wantCalls: 3, wantSleep: delays[:2]},
		{name: "gives up after the last delay", failures: []error{errStalled, errStalled, errStalled, errStalled}, wantErr: errStalled, wantCalls: 4, wantSleep: delays},
		{name: "fails at once on other errors", failures: []error{errRefused}, wantErr: errRefused, wantCalls: 1},
		{name: "stops at a later error that is not transient", failures: []error{errStalled, errRefused}, wantErr: errRefused, wantCalls: 2, wantSleep: delays[:1]},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var (
				calls int
				slept []time.Duration
			)

			write := func(b []byte, off int64) (int, error) {
				require.Equal(t, int64(4096), off, "every write is at the same offset")

				calls++
				if calls <= len(tc.failures) {
					return 0, tc.failures[calls-1]
				}

				return len(b), nil
			}

			n, err := retryWrite(write, make([]byte, 10), 4096, transient, delays, func(d time.Duration) { slept = append(slept, d) })

			require.ErrorIs(t, err, tc.wantErr)
			require.Equal(t, tc.wantCalls, calls)
			require.Equal(t, tc.wantSleep, slept)

			if tc.wantErr == nil {
				require.Equal(t, 10, n)
			}
		})
	}
}

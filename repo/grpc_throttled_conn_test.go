package repo

import (
	"io"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/repo/blob/throttling"
)

func TestThrottledConnPacesWritesAndReads(t *testing.T) {
	t.Parallel()

	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	const rate = 1 << 20 // 1 MiB/s

	tc := &throttledConn{Conn: client, up: newLimiter(rate), down: newLimiter(rate)}

	payload := make([]byte, 512<<10)

	go func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(server, int64(len(payload))))
		_, _ = server.Write(payload)
	}()

	start := time.Now()
	n, err := tc.Write(payload)
	require.NoError(t, err)
	require.Equal(t, len(payload), n)

	// the first burst is free, the rest is paced
	minimum := time.Duration(float64(len(payload)-throttledConnBurst) / rate * float64(time.Second))
	require.GreaterOrEqual(t, time.Since(start), minimum*9/10)

	start = time.Now()
	got, err := io.ReadAll(io.LimitReader(tc, int64(len(payload))))
	require.NoError(t, err)
	require.Len(t, got, len(payload))
	require.GreaterOrEqual(t, time.Since(start), minimum*9/10)
}

func TestThrottledDialerOnlyWhenLimited(t *testing.T) {
	t.Parallel()

	require.Nil(t, throttledDialer("dns:host:1", nil))
	require.Nil(t, throttledDialer("dns:host:1", &throttling.Limits{ReadsPerSecond: 5}))
	require.NotNil(t, throttledDialer("dns:host:1", &throttling.Limits{UploadBytesPerSecond: 1000}))
	require.NotNil(t, throttledDialer("unix:/tmp/sock", &throttling.Limits{DownloadBytesPerSecond: 1000}))
}

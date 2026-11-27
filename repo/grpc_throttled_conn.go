package repo

import (
	"context"
	"net"
	"strings"

	"github.com/pkg/errors"
	"golang.org/x/time/rate"

	"github.com/kopia/kopia/repo/blob/throttling"
)

// throttledConnBurst is the most a throttled connection reads or writes in
// one go, so the limiter can pace it.
const throttledConnBurst = 64 << 10

// throttledConn limits the bandwidth of a repository server connection.
//
// A server client has no blob storage of its own to throttle, so the
// connection's upload and download limits apply to the network connection
// itself: every byte, after wire compression, counts.
type throttledConn struct {
	net.Conn

	up, down *rate.Limiter
}

func newLimiter(bytesPerSecond float64) *rate.Limiter {
	if bytesPerSecond <= 0 {
		return nil
	}

	return rate.NewLimiter(rate.Limit(bytesPerSecond), throttledConnBurst)
}

func (c *throttledConn) Write(p []byte) (int, error) {
	if c.up == nil {
		return c.Conn.Write(p) //nolint:wrapcheck
	}

	written := 0

	for len(p) > 0 {
		n := min(len(p), throttledConnBurst)

		if err := c.up.WaitN(context.Background(), n); err != nil {
			return written, errors.Wrap(err, "upload throttling")
		}

		m, err := c.Conn.Write(p[:n])
		written += m

		if err != nil {
			return written, err //nolint:wrapcheck
		}

		p = p[n:]
	}

	return written, nil
}

func (c *throttledConn) Read(p []byte) (int, error) {
	if c.down == nil {
		return c.Conn.Read(p) //nolint:wrapcheck
	}

	n, err := c.Conn.Read(p[:min(len(p), throttledConnBurst)])
	if n > 0 {
		if werr := c.down.WaitN(context.Background(), n); werr != nil && err == nil {
			err = errors.Wrap(werr, "download throttling")
		}
	}

	return n, err //nolint:wrapcheck
}

// throttledDialer returns a gRPC dialer whose connections honor the upload
// and download limits, or nil when there are none.
func throttledDialer(uri string, limits *throttling.Limits) func(ctx context.Context, addr string) (net.Conn, error) {
	if limits == nil || (limits.UploadBytesPerSecond <= 0 && limits.DownloadBytesPerSecond <= 0) {
		return nil
	}

	network := "tcp"
	if strings.HasPrefix(uri, "unix:") {
		network = "unix"
	}

	return func(ctx context.Context, addr string) (net.Conn, error) {
		var d net.Dialer

		conn, err := d.DialContext(ctx, network, addr)
		if err != nil {
			return nil, err //nolint:wrapcheck
		}

		return &throttledConn{
			Conn: conn,
			up:   newLimiter(limits.UploadBytesPerSecond),
			down: newLimiter(limits.DownloadBytesPerSecond),
		}, nil
	}
}

// ConnectionThrottling returns the throttling limits saved with the
// repository connection in configFile.
func ConnectionThrottling(configFile string) (throttling.Limits, error) {
	lc, err := LoadConfigFromFile(configFile)
	if err != nil {
		return throttling.Limits{}, err
	}

	if lc.Throttling == nil {
		return throttling.Limits{}, nil
	}

	return *lc.Throttling, nil
}

// SetConnectionThrottling saves throttling limits with the repository
// connection in configFile. A repository server connection applies the
// upload and download limits the next time it connects.
func SetConnectionThrottling(configFile string, limits throttling.Limits) error {
	lc, err := LoadConfigFromFile(configFile)
	if err != nil {
		return err
	}

	lc.Throttling = &limits

	return lc.writeToFile(configFile)
}

package repo

import (
	"io"
	"syscall"

	"github.com/pkg/errors"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// IsConnectionLost reports whether an error means that the connection to a
// repository server broke or could not be made, such as when the computer
// changed networks or the server restarted, rather than that the server
// refused a request. What the repository already holds is unaffected, and the
// work can be done again over a new connection.
func IsConnectionLost(err error) bool {
	if err == nil {
		return false
	}

	// a broken session stream is reported as io.EOF
	for _, target := range []error{io.EOF, io.ErrUnexpectedEOF, ErrServerUnreachable, errGRPCConnectionClosed, syscall.ECONNRESET, syscall.ECONNABORTED, syscall.EPIPE} {
		if errors.Is(err, target) {
			return true
		}
	}

	return status.Code(err) == codes.Unavailable
}

package repo

import (
	"io"
	"syscall"
	"testing"

	"github.com/pkg/errors"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestIsConnectionLost(t *testing.T) {
	lost := []error{
		// as a broken session stream reports it, wrapped by the image run
		errors.Wrap(errors.Wrap(io.EOF, "EOF"), "error waiting for async writes"),
		io.ErrUnexpectedEOF,
		errors.Wrapf(ErrServerUnreachable, "timed out waiting for gRPC connection readiness"),
		errors.Wrap(errGRPCConnectionClosed, "unable to establish session"),
		errors.Wrap(syscall.ECONNRESET, "write"),
		status.Error(codes.Unavailable, "connection refused"),
		errors.Wrap(status.Error(codes.Unavailable, "transport is closing"), "flush"),
	}

	for _, err := range lost {
		require.True(t, IsConnectionLost(err), "%v", err)
	}

	kept := []error{
		nil,
		errors.New("access denied"),
		status.Error(codes.PermissionDenied, "not allowed"),
		errors.Wrap(syscall.ENOSPC, "write"),
	}

	for _, err := range kept {
		require.False(t, IsConnectionLost(err), "%v", err)
	}
}

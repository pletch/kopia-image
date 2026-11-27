package image

import (
	"io"
	"syscall"
	"testing"

	"github.com/pkg/errors"
	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/repo"
)

func TestLocalReadErrorIsNotConnectionLost(t *testing.T) {
	short := localReadError(io.ErrUnexpectedEOF, 4096, 1<<20)
	require.ErrorContains(t, short, "short read")
	require.False(t, repo.IsConnectionLost(short), "a short local read is not a lost connection")

	other := localReadError(syscall.EIO, 4096, 1<<20)
	require.ErrorIs(t, other, syscall.EIO)

	require.True(t, repo.IsConnectionLost(errors.Wrap(io.ErrUnexpectedEOF, "server stream")))
}

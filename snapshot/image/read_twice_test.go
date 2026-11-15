package image

import (
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/pkg/errors"
	"github.com/stretchr/testify/require"
)

// readSequence serves the n-th open from contents[n], repeating the last one.
type readSequence struct {
	contents [][]byte
	opens    int
}

func (s *readSequence) open() (io.ReadCloser, error) {
	c := s.contents[min(s.opens, len(s.contents)-1)]
	s.opens++

	return io.NopCloser(bytes.NewReader(c)), nil
}

func consumeAll(r io.Reader) ([]byte, error) {
	return io.ReadAll(r)
}

func TestReadTwice_stable(t *testing.T) {
	s := &readSequence{contents: [][]byte{[]byte("stable")}}

	got, err := ReadTwice(context.Background(), 3, s.open, consumeAll)
	require.NoError(t, err)
	require.Equal(t, []byte("stable"), got)
	require.Equal(t, 2, s.opens)
}

func TestReadTwice_tornReadRetried(t *testing.T) {
	// first attempt: the second read sees a write; second attempt: both match
	s := &readSequence{contents: [][]byte{[]byte("before"), []byte("torn!!"), []byte("after"), []byte("after")}}

	got, err := ReadTwice(context.Background(), 3, s.open, consumeAll)
	require.NoError(t, err)
	require.Equal(t, []byte("after"), got)
	require.Equal(t, 4, s.opens)
}

func TestReadTwice_partialConsumeStillHashesAll(t *testing.T) {
	s := &readSequence{contents: [][]byte{[]byte("same-prefix-A"), []byte("same-prefix-B")}}

	_, err := ReadTwice(context.Background(), 1, s.open, func(r io.Reader) (string, error) {
		b := make([]byte, 4)
		_, err := io.ReadFull(r, b)

		return string(b), err
	})
	require.ErrorIs(t, err, ErrUnstableSource)
}

func TestReadTwice_unstable(t *testing.T) {
	n := 0

	open := func() (io.ReadCloser, error) {
		n++
		return io.NopCloser(bytes.NewReader([]byte{byte(n)})), nil
	}

	_, err := ReadTwice(context.Background(), 3, open, consumeAll)
	require.ErrorIs(t, err, ErrUnstableSource)
	require.Equal(t, 6, n)
}

func TestReadTwice_consumeError(t *testing.T) {
	errBoom := errors.New("boom")
	s := &readSequence{contents: [][]byte{[]byte("x")}}

	_, err := ReadTwice(context.Background(), 3, s.open, func(io.Reader) (int, error) { return 0, errBoom })
	require.ErrorIs(t, err, errBoom)
	require.Equal(t, 2, s.opens)
}

func TestReadTwice_openError(t *testing.T) {
	errBoom := errors.New("boom")

	_, err := ReadTwice(context.Background(), 3, func() (io.ReadCloser, error) { return nil, errBoom }, consumeAll)
	require.ErrorIs(t, err, errBoom)
}

func TestReadTwice_canceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	s := &readSequence{contents: [][]byte{[]byte("x")}}

	_, err := ReadTwice(ctx, 3, s.open, consumeAll)
	require.ErrorIs(t, err, context.Canceled)
	require.Zero(t, s.opens)
}

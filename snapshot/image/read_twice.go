package image

import (
	"bytes"
	"context"
	"crypto/sha256"
	stderrors "errors"
	"io"

	"github.com/pkg/errors"
)

// ErrUnstableSource is returned by ReadTwice when no two consecutive reads of
// the source matched.
var ErrUnstableSource = errors.New("source changed between reads")

// ReadTwice reads a source that has no shadow copy, such as an EFI system
// partition, so that a write during the read cannot produce a torn image.
//
// Each attempt reads the source twice: the first read is only hashed, and
// the second is passed to consume while being hashed. When the SHA-256
// digests match, ReadTwice returns the result of consume. Otherwise it
// discards that result and tries again, up to attempts times, and then
// returns ErrUnstableSource. A discarded result written to a repository
// leaves unreferenced contents, which maintenance removes.
//
// open must return a new reader positioned at the start of the source on
// every call. consume need not read to the end; the rest is still hashed.
func ReadTwice[T any](ctx context.Context, attempts int, open func() (io.ReadCloser, error), consume func(io.Reader) (T, error)) (T, error) {
	var zero T

	for range attempts {
		if err := ctx.Err(); err != nil {
			return zero, errors.Wrap(err, "read canceled")
		}

		first, err := hashSource(open, func(r io.Reader) error {
			_, err := io.Copy(io.Discard, r)
			return errors.Wrap(err, "first read")
		})
		if err != nil {
			return zero, err
		}

		var result T

		second, err := hashSource(open, func(r io.Reader) error {
			v, err := consume(r)
			if err != nil {
				return err
			}

			result = v

			_, err = io.Copy(io.Discard, r)

			return errors.Wrap(err, "second read")
		})
		if err != nil {
			return zero, err
		}

		if bytes.Equal(first, second) {
			return result, nil
		}
	}

	return zero, ErrUnstableSource
}

func hashSource(open func() (io.ReadCloser, error), read func(io.Reader) error) (digest []byte, err error) {
	rc, err := open()
	if err != nil {
		return nil, errors.Wrap(err, "unable to open source")
	}

	defer func() {
		err = stderrors.Join(err, rc.Close())
	}()

	h := sha256.New()

	if err := read(io.TeeReader(rc, h)); err != nil {
		return nil, err
	}

	return h.Sum(nil), nil
}

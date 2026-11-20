package imagerestore

import (
	"time"
)

// retryWrite writes b at off, and again after each of delays while the write
// fails with an error that transient reports as passing, such as a timeout.
// Writing the same bytes at the same offset again is harmless.
func retryWrite(writeAt func(b []byte, off int64) (int, error), b []byte, off int64,
	transient func(error) bool, delays []time.Duration, sleep func(time.Duration),
) (int, error) {
	n, err := writeAt(b, off)

	for _, d := range delays {
		if err == nil || !transient(err) {
			break
		}

		sleep(d)

		n, err = writeAt(b, off)
	}

	return n, err
}

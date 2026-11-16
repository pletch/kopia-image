package blockdev

import "unsafe"

// unbufferedAlignment is the alignment of offsets, lengths and buffer
// addresses for unbuffered device reads. It is a multiple of every logical
// sector size in use.
const unbufferedAlignment = 4096

// AlignedBuffer returns a buffer of n bytes whose address is aligned for
// unbuffered device reads.
func AlignedBuffer(n int) []byte {
	raw := make([]byte, n+unbufferedAlignment)
	shift := (unbufferedAlignment - int(uintptr(unsafe.Pointer(&raw[0]))%unbufferedAlignment)) % unbufferedAlignment //nolint:gosec

	return raw[shift : shift+n : shift+n]
}

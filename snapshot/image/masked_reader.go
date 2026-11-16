package image

import (
	"io"
	"sort"
	"sync/atomic"

	"github.com/pkg/errors"
)

// MaskedReader presents a volume of a fixed size in which the bytes of the
// masked extents read as zeros. Masked ranges are never read from the source,
// so free space costs no I/O.
//
// Every source read starts and ends either at a requested boundary or at a
// mask extent boundary. When callers request aligned ranges and the mask is
// cluster-aligned, source reads stay aligned, as unbuffered device I/O requires.
//
// ReadAt is safe for concurrent use; Read is not.
type MaskedReader struct {
	src  io.ReaderAt
	size int64
	mask []Extent // normalized and clipped to [0, size)

	sourceBytesRead atomic.Int64

	readOffset int64 // used by Read
}

// NewMaskedReader returns a reader of size bytes that reads src except within
// the mask extents. Mask extents may overlap and extend beyond size.
func NewMaskedReader(src io.ReaderAt, size int64, mask []Extent) (*MaskedReader, error) {
	if size < 0 {
		return nil, errors.Errorf("invalid size %v", size)
	}

	normalized, err := NormalizeExtents(mask)
	if err != nil {
		return nil, errors.Wrap(err, "invalid mask")
	}

	clipped := normalized[:0]

	for _, e := range normalized {
		if e.Start >= size {
			break
		}

		e.Length = min(e.End(), size) - e.Start
		clipped = append(clipped, e)
	}

	return &MaskedReader{src: src, size: size, mask: clipped}, nil
}

// Size returns the size of the volume in bytes.
func (r *MaskedReader) Size() int64 {
	return r.size
}

// MaskedBytes returns the number of bytes that read as zeros because of the mask.
func (r *MaskedReader) MaskedBytes() int64 {
	return TotalLength(r.mask)
}

// SourceBytesRead returns the number of bytes read from the source so far.
func (r *MaskedReader) SourceBytesRead() int64 {
	return r.sourceBytesRead.Load()
}

// IsMasked reports whether [off, off+length) lies entirely within the mask,
// so that it reads as zeros without touching the source.
func (r *MaskedReader) IsMasked(off, length int64) bool {
	// the mask is normalized, so a masked range lies within a single extent
	i := sort.Search(len(r.mask), func(i int) bool { return r.mask[i].End() > off })

	return i < len(r.mask) && r.mask[i].Start <= off && r.mask[i].End() >= off+length
}

// ReadAt implements io.ReaderAt.
func (r *MaskedReader) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.Errorf("invalid offset %v", off)
	}

	if off >= r.size {
		return 0, io.EOF
	}

	end := off + min(int64(len(p)), r.size-off)

	// first mask extent that ends after off
	i := sort.Search(len(r.mask), func(i int) bool { return r.mask[i].End() > off })

	for pos := off; pos < end; {
		if i < len(r.mask) && r.mask[i].Start <= pos {
			next := min(r.mask[i].End(), end)
			clear(p[pos-off : next-off])
			pos = next
			i++

			continue
		}

		next := end
		if i < len(r.mask) {
			next = min(r.mask[i].Start, end)
		}

		if err := r.readSource(p[pos-off:next-off], pos); err != nil {
			return int(pos - off), err
		}

		pos = next
	}

	n := int(end - off)
	if n < len(p) {
		return n, io.EOF
	}

	return n, nil
}

func (r *MaskedReader) readSource(p []byte, off int64) error {
	n, err := r.src.ReadAt(p, off)
	r.sourceBytesRead.Add(int64(n))

	if n == len(p) {
		// io.ReaderAt may return io.EOF with a full read at the end of the source
		return nil
	}

	if err == nil || errors.Is(err, io.EOF) {
		err = io.ErrUnexpectedEOF
	}

	return errors.Wrapf(err, "error reading %v bytes at offset %v", len(p), off)
}

// Read implements io.Reader, reading sequentially from the start of the volume.
func (r *MaskedReader) Read(p []byte) (int, error) {
	n, err := r.ReadAt(p, r.readOffset)
	r.readOffset += int64(n)

	if errors.Is(err, io.EOF) && n > 0 {
		return n, nil
	}

	return n, err
}

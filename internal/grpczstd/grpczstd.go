// Package grpczstd registers a fast zstd compressor with gRPC, so repository
// server clients can compress the session stream on the wire.
//
// A server client sends contents uncompressed and the server compresses
// them for storage, so without wire compression every image chunk crossed
// the network at full size. Importing this package registers the codec on
// both sides: the client asks for it with grpc.UseCompressor(Name), and the
// server answers in kind.
package grpczstd

import (
	"io"
	"sync"

	"github.com/klauspost/compress/zstd"
	"google.golang.org/grpc/encoding"
)

// Name is the gRPC content coding of the codec.
const Name = "zstd"

// maxDecodedSize bounds what one message may decompress to, well above
// gRPC's own message size limit, so a malformed stream can't exhaust memory.
const maxDecodedSize = 256 << 20

//nolint:gochecknoinits
func init() {
	encoding.RegisterCompressor(&compressor{})
}

type compressor struct {
	encoders sync.Pool
	decoders sync.Pool
}

func (c *compressor) Name() string { return Name }

func (c *compressor) Compress(w io.Writer) (io.WriteCloser, error) {
	enc, _ := c.encoders.Get().(*zstd.Encoder)
	if enc == nil {
		var err error

		enc, err = zstd.NewWriter(nil,
			zstd.WithEncoderLevel(zstd.SpeedFastest),
			zstd.WithEncoderConcurrency(1),
		)
		if err != nil {
			return nil, err //nolint:wrapcheck
		}
	}

	enc.Reset(w)

	return &writer{Encoder: enc, pool: &c.encoders}, nil
}

func (c *compressor) Decompress(r io.Reader) (io.Reader, error) {
	dec, _ := c.decoders.Get().(*zstd.Decoder)
	if dec == nil {
		var err error

		dec, err = zstd.NewReader(nil,
			zstd.WithDecoderConcurrency(1),
			zstd.WithDecoderMaxMemory(maxDecodedSize),
		)
		if err != nil {
			return nil, err //nolint:wrapcheck
		}
	}

	if err := dec.Reset(r); err != nil {
		c.decoders.Put(dec)
		return nil, err //nolint:wrapcheck
	}

	return &reader{Decoder: dec, pool: &c.decoders}, nil
}

type writer struct {
	*zstd.Encoder

	pool *sync.Pool
}

func (w *writer) Close() error {
	err := w.Encoder.Close()

	w.pool.Put(w.Encoder)

	return err //nolint:wrapcheck
}

type reader struct {
	*zstd.Decoder

	pool *sync.Pool
	once sync.Once
}

// Close returns the decoder to the pool; gRPC calls it exactly once.
func (r *reader) Close() error {
	r.once.Do(func() {
		_ = r.Decoder.Reset(nil)
		r.pool.Put(r.Decoder)
	})

	return nil
}

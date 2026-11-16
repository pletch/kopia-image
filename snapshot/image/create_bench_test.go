package image

import (
	"bytes"
	"io"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/internal/repotesting"
	"github.com/kopia/kopia/repo/object"
)

// BenchmarkCreate_steadyState measures a repeated image of unchanged data,
// where every chunk is already in the repository: the per-run local cost of
// reading, zero detection and hashing. Comparing zeros with random data shows
// what zero-chunk skipping could save.
func BenchmarkCreate_steadyState(b *testing.B) {
	const size = 256 << 20

	for _, tc := range []struct {
		name string
		data []byte
	}{
		{"zeros", make([]byte, size)},
		{"random", randomBytes(3, size)},
	} {
		b.Run(tc.name, func(b *testing.B) {
			ctx, env := repotesting.NewEnvironment(b, repotesting.FormatNotImportant)

			opt := &CreateOptions{
				Source:      testSource(b),
				Layout:      testLayout(),
				Volumes:     []VolumeSource{{PartitionNumber: 3, Label: "C", Kind: VolumeKindNTFS, Size: size, Open: bytesSource(tc.data)}},
				Compressor:  "zstd-fastest",
				AsyncWrites: runtime.GOMAXPROCS(0),
			}

			// the first image uploads the chunks; the measured ones find them all present
			_, _, err := Create(ctx, env.RepositoryWriter, opt)
			require.NoError(b, err)

			b.SetBytes(size)
			b.ResetTimer()

			for range b.N {
				_, _, err := Create(ctx, env.RepositoryWriter, opt)
				require.NoError(b, err)
			}
		})
	}
}

// BenchmarkWriteImageObject compares writing unchanged data chunk by chunk,
// which skips hashing zero chunks, with kopia's streaming FIXED-4M writer,
// which hashes every chunk.
func BenchmarkWriteImageObject(b *testing.B) {
	const size = 1 << 30

	for _, data := range []struct {
		name string
		data []byte
	}{
		{"zeros", make([]byte, size)},
		{"random", randomBytes(4, size)},
	} {
		for _, writer := range []string{"chunked", "streaming"} {
			b.Run(data.name+"/"+writer, func(b *testing.B) {
				ctx, env := repotesting.NewEnvironment(b, repotesting.FormatNotImportant)
				opt := &CreateOptions{Compressor: "zstd-fastest", AsyncWrites: runtime.GOMAXPROCS(0)}

				write := func() {
					if writer == "chunked" {
						_, _, err := opt.writeImageObject(ctx, env.RepositoryWriter, bytes.NewReader(data.data), size, "p3-C.img")
						require.NoError(b, err)

						return
					}

					w := env.RepositoryWriter.NewObjectWriter(ctx, object.WriterOptions{
						Splitter: imageSplitter, Compressor: opt.Compressor, AsyncWrites: opt.AsyncWrites,
					})

					// as the previous Create did: read each chunk into a buffer, then write it
					r := bytes.NewReader(data.data)
					buf := make([]byte, DefaultChunkSize)

					for {
						n, err := io.ReadFull(r, buf)
						if n > 0 {
							_, werr := w.Write(buf[:n])
							require.NoError(b, werr)
						}

						if err != nil {
							break
						}
					}

					_, err := w.Result()
					require.NoError(b, err)
					require.NoError(b, w.Close())
				}

				write() // upload once; the measured writes find every chunk present

				b.SetBytes(size)
				b.ResetTimer()

				for range b.N {
					write()
				}
			})
		}
	}
}

// BenchmarkWriteImageObject_freeSpace measures 1 GiB of free space as an NTFS
// image reads it: through a MaskedReader whose mask covers everything.
func BenchmarkWriteImageObject_freeSpace(b *testing.B) {
	const size = 1 << 30

	for _, writer := range []string{"chunked", "streaming"} {
		b.Run(writer, func(b *testing.B) {
			ctx, env := repotesting.NewEnvironment(b, repotesting.FormatNotImportant)
			opt := &CreateOptions{Compressor: "zstd-fastest", AsyncWrites: runtime.GOMAXPROCS(0)}

			write := func() {
				mr, err := NewMaskedReader(failingReaderAt{io.ErrUnexpectedEOF}, size, []Extent{{Start: 0, Length: size}})
				require.NoError(b, err)

				if writer == "chunked" {
					_, _, err := opt.writeImageObject(ctx, env.RepositoryWriter, maskedReadCloser{mr}, size, "p3-C.img")
					require.NoError(b, err)

					return
				}

				w := env.RepositoryWriter.NewObjectWriter(ctx, object.WriterOptions{
					Splitter: imageSplitter, Compressor: opt.Compressor, AsyncWrites: opt.AsyncWrites,
				})

				buf := make([]byte, DefaultChunkSize)

				for {
					n, err := io.ReadFull(mr, buf)
					if n > 0 {
						_, werr := w.Write(buf[:n])
						require.NoError(b, werr)
					}

					if err != nil {
						break
					}
				}

				_, err = w.Result()
				require.NoError(b, err)
				require.NoError(b, w.Close())
			}

			write()

			b.SetBytes(size)
			b.ResetTimer()

			for range b.N {
				write()
			}
		})
	}
}

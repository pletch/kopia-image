package ntfsfs

import (
	"bytes"
	"context"
	"encoding/binary"
	"sync/atomic"
	"testing"

	"github.com/pkg/errors"
	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/fs"
	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/object"
)

var errOffline = errors.New("repository is offline")

// flakyRepository fails object reads while offline is set.
type flakyRepository struct {
	repo.Repository

	offline atomic.Bool
}

func (r *flakyRepository) OpenObject(ctx context.Context, id object.ID) (object.Reader, error) {
	if r.offline.Load() {
		return nil, errOffline
	}

	return r.Repository.OpenObject(ctx, id) //nolint:wrapcheck
}

func TestLazyVolume_retriesAfterFailedOpen(t *testing.T) {
	ctx, env, root := createTestImageSnapshot(t)

	rep := &flakyRepository{Repository: env.Repository}
	rep.offline.Store(true)

	wrapped := WithBrowsableVolumes(ctx, env.Repository, root)

	vol := wrapped.(*browsableRoot).volumes[0] //nolint:forcetypeassert
	vol.rep = rep

	_, err := vol.Child(ctx, "Folder A")
	require.ErrorIs(t, err, errOffline)

	// the failure is remembered only briefly
	rep.offline.Store(false)

	_, err = vol.Child(ctx, "Folder A")
	require.ErrorIs(t, err, errOffline, "a recent failure is not retried at once")

	vol.mu.Lock()
	vol.failedAt = vol.failedAt.Add(-2 * openRetryInterval)
	vol.mu.Unlock()

	e, err := vol.Child(ctx, "Folder A")
	require.NoError(t, err, "the volume opens once the repository is back")
	require.True(t, e.IsDir())
}

func TestReleaseVolumes(t *testing.T) {
	ctx, env, root := createTestImageSnapshot(t)

	wrapped := WithBrowsableVolumes(ctx, env.Repository, root)
	lazy := wrapped.(*browsableRoot).volumes[0] //nolint:forcetypeassert

	// releasing volumes that were never opened, or other directories, does nothing
	ReleaseVolumes(wrapped)
	ReleaseVolumes(root)

	volDir, ok := mustChild(ctx, t, wrapped, "p2-C").(fs.Directory)
	require.True(t, ok)

	const helloPath = "Folder A/Folder B/Hello world text document.txt"

	hello := lookup(ctx, t, volDir, helloPath)

	lazy.mu.Lock()
	opened := lazy.vol
	lazy.mu.Unlock()

	require.NotNil(t, opened)
	require.Positive(t, cachedChunks(opened))

	ReleaseVolumes(wrapped)

	lazy.mu.Lock()
	require.Nil(t, lazy.vol)
	lazy.mu.Unlock()

	require.Zero(t, cachedChunks(opened), "the chunk cache is released")

	// an entry obtained before the release still reads, and the folder reopens
	require.Equal(t, "Hello world!", string(readFile(ctx, t, hello)))
	require.Equal(t, "Hello world!", string(readFile(ctx, t, lookup(ctx, t, volDir, helloPath))))
}

func mustChild(ctx context.Context, t *testing.T, dir fs.Directory, name string) fs.Entry {
	t.Helper()

	e, err := dir.Child(ctx, name)
	require.NoError(t, err)

	return e
}

func cachedChunks(v *Volume) int {
	v.cr.mu.Lock()
	defer v.cr.mu.Unlock()

	return v.cr.lru.Len()
}

func TestParseRecord_rejectsInvalidSizes(t *testing.T) {
	for _, size := range []int{0, 3, 47, 511, 1000, 1025} {
		_, err := parseRecord(make([]byte, size), 0)
		require.ErrorContains(t, err, "invalid record size", "size %v", size)
	}
}

// validRecord returns a minimal well-formed 1024-byte MFT record with one
// resident and one non-resident attribute.
func validRecord() []byte {
	const size = 1024

	le := binary.LittleEndian
	buf := make([]byte, size)

	copy(buf, "FILE")
	le.PutUint16(buf[4:], 0x30) // update sequence array offset
	le.PutUint16(buf[6:], 3)    // one sequence number and one entry per sector
	le.PutUint16(buf[0x10:], 1) // sequence number
	le.PutUint16(buf[0x14:], 0x38)
	le.PutUint16(buf[0x16:], 1) // in use
	le.PutUint32(buf[0x1C:], size)
	le.PutUint32(buf[0x2C:], 30) // record number

	off := 0x38

	// resident attribute 0x10 with an 8-byte value
	le.PutUint32(buf[off:], 0x10)
	le.PutUint32(buf[off+4:], 0x20)
	le.PutUint32(buf[off+0x10:], 8)
	le.PutUint16(buf[off+0x14:], 0x18)
	off += 0x20

	// non-resident attribute 0x80 with the run list 0x21 0x10 0x00 0x01, end
	le.PutUint32(buf[off:], 0x80)
	le.PutUint32(buf[off+4:], 0x48)
	buf[off+8] = 1
	le.PutUint64(buf[off+0x18:], 15)
	le.PutUint16(buf[off+0x20:], 0x40)
	le.PutUint64(buf[off+0x30:], 16*4096)
	copy(buf[off+0x40:], []byte{0x21, 0x10, 0x00, 0x01, 0x00})
	off += 0x48

	le.PutUint32(buf[off:], attrTypeEnd)
	le.PutUint32(buf[0x18:], uint32(off+8)) //nolint:gosec

	// apply the update sequence: the last two bytes of each sector move to the array
	usn := []byte{0xAB, 0xCD}
	copy(buf[0x30:], usn)

	for i := 1; i <= 2; i++ {
		end := i*fixupStride - usnSize
		copy(buf[0x30+usnSize*i:], buf[end:end+usnSize])
		copy(buf[end:], usn)
	}

	return buf
}

func TestParseRecord_validRecord(t *testing.T) {
	rec, err := parseRecord(validRecord(), 30)
	require.NoError(t, err)
	require.Len(t, rec.attributes, 2)
	require.Len(t, rec.attributes[0].value, 8)

	runs, err := decodeRuns(rec.attributes[1].runList)
	require.NoError(t, err)
	require.Equal(t, []run{{lcn: 0x100, length: 16}}, runs)
}

// FuzzParseRecord checks that the strict record parser never panics and that
// what it returns stays inside the record.
func FuzzParseRecord(f *testing.F) {
	f.Add(validRecord(), int64(30))
	f.Add(make([]byte, 1024), int64(0))
	f.Add(bytes.Repeat([]byte("FILE"), 256), int64(5))
	f.Add([]byte("FILE"), int64(0))

	f.Fuzz(func(t *testing.T, data []byte, n int64) {
		buf := bytes.Clone(data)

		rec, err := parseRecord(buf, n)
		if err != nil {
			return
		}

		for i := range rec.attributes {
			a := &rec.attributes[i]

			require.LessOrEqual(t, len(a.value), len(buf))
			require.LessOrEqual(t, len(a.runList), len(buf))

			if a.nonResident {
				// the run list of an accepted record is untrusted too
				_, _ = decodeRuns(a.runList)
			}
		}

		require.NotNil(t, rec)
		_ = findAttribute(rec, attrTypeData)
	})
}

// FuzzDecodeRuns checks that the run list decoder never panics, never loops
// and returns only runs of positive length.
func FuzzDecodeRuns(f *testing.F) {
	f.Add([]byte{0x21, 0x10, 0x00, 0x01, 0x00})
	f.Add([]byte{0x01, 0x05, 0x11, 0x03, 0xFF, 0x00})
	f.Add([]byte{0x88, 1, 2, 3, 4, 5, 6, 7, 8, 1, 2, 3, 4, 5, 6, 7, 0x80, 0x00})
	f.Add([]byte{0x21})
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, data []byte) {
		runs, err := decodeRuns(data)
		if err != nil {
			return
		}

		// every run takes at least two bytes of the list
		require.LessOrEqual(t, len(runs), len(data)/2)

		for _, r := range runs {
			require.Positive(t, r.length)

			if r.sparse {
				require.Zero(t, r.lcn)
			}
		}
	})
}

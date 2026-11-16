package imagerestore

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/internal/repotesting"
	"github.com/kopia/kopia/repo/object"
)

func TestWriteObject_unexpectedLengthWritesNothing(t *testing.T) {
	ctx, env := repotesting.NewEnvironment(t, repotesting.FormatNotImportant)

	data := testImage()
	oid := writeTestObject(ctx, t, env.RepositoryWriter, data)

	for _, expected := range []int64{int64(len(data)) - 1, int64(len(data)) + 1, chunk} {
		disk := &memDisk{data: bytes.Repeat([]byte{0xee}, len(data))}

		_, err := WriteObject(ctx, env.Repository, oid, disk, 0, Options{ExpectedLength: expected, Zero: disk.zero})
		require.ErrorIs(t, err, ErrUnexpectedLength)
		require.Equal(t, bytes.Repeat([]byte{0xee}, len(data)), disk.data, "the target is untouched")
		require.Zero(t, disk.zeros)
	}

	disk := &memDisk{data: make([]byte, len(data))}

	_, err := WriteObject(ctx, env.Repository, oid, disk, 0, Options{ExpectedLength: int64(len(data))})
	require.NoError(t, err)
	require.Equal(t, data, disk.data)
}

func indexJSON(t *testing.T, stream string, entries ...object.IndirectObjectEntry) []byte {
	t.Helper()

	b, err := json.Marshal(map[string]any{"stream": stream, "entries": entries})
	require.NoError(t, err)

	return b
}

func TestDecodeIndex(t *testing.T) {
	oid := object.EmptyID

	entries, length, err := decodeIndex(bytes.NewReader(indexJSON(t, indirectStreamType,
		object.IndirectObjectEntry{Start: 0, Length: chunk, Object: oid},
		object.IndirectObjectEntry{Start: chunk, Length: 100, Object: oid})))
	require.NoError(t, err)
	require.Len(t, entries, 2)
	require.Equal(t, int64(chunk+100), length)
}

func TestDecodeIndex_rejectsInvalidChunkLists(t *testing.T) {
	oid := object.EmptyID

	cases := map[string][]byte{
		"not JSON":         []byte("{"),
		"other stream":     indexJSON(t, "other", object.IndirectObjectEntry{Length: 1, Object: oid}),
		"gap":              indexJSON(t, indirectStreamType, object.IndirectObjectEntry{Start: 1, Length: 1, Object: oid}),
		"overlap":          indexJSON(t, indirectStreamType, object.IndirectObjectEntry{Length: 2, Object: oid}, object.IndirectObjectEntry{Start: 1, Length: 2, Object: oid}),
		"empty chunk":      indexJSON(t, indirectStreamType, object.IndirectObjectEntry{Length: 0, Object: oid}),
		"negative chunk":   indexJSON(t, indirectStreamType, object.IndirectObjectEntry{Length: -5, Object: oid}),
		"oversized chunk":  indexJSON(t, indirectStreamType, object.IndirectObjectEntry{Length: MaxChunkLength + 1, Object: oid}),
		"enormous chunk":   indexJSON(t, indirectStreamType, object.IndirectObjectEntry{Length: 1 << 62, Object: oid}),
		"oversized second": indexJSON(t, indirectStreamType, object.IndirectObjectEntry{Length: 1, Object: oid}, object.IndirectObjectEntry{Start: 1, Length: 1 << 40, Object: oid}),
	}

	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			_, _, err := decodeIndex(bytes.NewReader(data))
			require.Error(t, err)
		})
	}
}

// FuzzDecodeIndex checks that every chunk list decodeIndex accepts tiles the
// object and holds no chunk a reader could not afford to allocate.
func FuzzDecodeIndex(f *testing.F) {
	f.Add([]byte(`{"stream":"kopia:indirect","entries":[{"s":0,"l":4194304,"o":"aabb"},{"s":4194304,"l":7,"o":"aabb"}]}`))
	f.Add([]byte(`{"stream":"kopia:indirect","entries":[{"l":9223372036854775807,"o":"aabb"},{"s":9223372036854775807,"l":9223372036854775807,"o":"aabb"}]}`))
	f.Add([]byte(`{"stream":"kopia:indirect","entries":[{"s":-1,"l":1}]}`))
	f.Add([]byte(`{}`))

	f.Fuzz(func(t *testing.T, data []byte) {
		entries, length, err := decodeIndex(bytes.NewReader(data))
		if err != nil {
			return
		}

		var end int64

		for _, e := range entries {
			require.Equal(t, end, e.Start)
			require.Positive(t, e.Length)
			require.LessOrEqual(t, e.Length, int64(MaxChunkLength))

			end += e.Length
		}

		require.Equal(t, end, length)
	})
}

func TestRestoreDisk_zeroesTheEndOfPartitions(t *testing.T) {
	ctx, env, s, contents := createImageSnapshot(t)

	// shorten the NTFS volume's partition use: restore into a dirty file and
	// check that nothing of the old contents survives inside any partition
	path := filepath.Join(t.TempDir(), "disk.img")
	require.NoError(t, os.WriteFile(path, bytes.Repeat([]byte{0xee}, 128<<20), 0o600))

	target, err := OpenTarget(path, 0, 512)
	require.NoError(t, err)

	_, err = RestoreDisk(ctx, env.Repository, s, target, DiskOptions{Volumes: s.Metadata.Volumes})
	require.NoError(t, err)
	require.NoError(t, target.Close())

	verifyRestoredDisk(t, path, s, contents)

	disk, err := os.ReadFile(path)
	require.NoError(t, err)

	shorter := 0

	for _, v := range s.Metadata.Volumes {
		p, ok := s.Layout.Partition(v.PartitionNumber)
		require.True(t, ok)

		if v.Size == p.Length {
			continue
		}

		shorter++

		tail := disk[p.Offset+v.Size : p.Offset+p.Length]
		require.Equal(t, make([]byte, len(tail)), tail, "the end of partition %v holds old data", p.Number)
	}

	require.Positive(t, shorter, "the test image needs a volume shorter than its partition")
}

func TestOpenTargetReadOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	require.NoError(t, os.WriteFile(path, bytes.Repeat([]byte{0xee}, 1<<20), 0o600))

	target, err := OpenTargetReadOnly(path, 512)
	require.NoError(t, err)

	defer target.Close()

	require.Equal(t, int64(1<<20), target.Size())
	require.False(t, target.IsZeroed())

	b := make([]byte, 512)
	_, err = target.ReadAt(b, 0)
	require.NoError(t, err)

	_, err = target.WriteAt(b, 0)
	require.Error(t, err, "a read-only target cannot be written")

	_, err = OpenTargetReadOnly(filepath.Join(t.TempDir(), "missing.img"), 512)
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestCheckNewTargetPath(t *testing.T) {
	require.NoError(t, CheckNewTargetPath(filepath.Join(t.TempDir(), "disk.img")))

	if runtime.GOOS == "windows" {
		t.Skip("device names are not paths on Windows")
	}

	for _, path := range []string{"/dev/sdx-no-such-disk", "/dev/disk/by-id/no-such-disk", "/dev/../dev/sdx"} {
		require.ErrorContains(t, CheckNewTargetPath(path), "does not exist", path)

		_, err := OpenTarget(path, 1<<20, 512)
		require.ErrorContains(t, err, "does not exist", path)
	}

	require.NoError(t, CheckNewTargetPath("/devices/disk.img"))
}

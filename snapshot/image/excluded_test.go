package image

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/fs"
	"github.com/kopia/kopia/internal/repotesting"
	"github.com/kopia/kopia/snapshot/snapshotfs"
)

func TestExcludedFilesRoundTrip(t *testing.T) {
	files := []string{`b\y.bin`, `a\x.bin`, `a\sub\z 1.bin`}

	data := EncodeExcludedFiles(files)
	require.Equal(t, "a\\sub\\z 1.bin\na\\x.bin\nb\\y.bin\n", string(data))

	got, err := DecodeExcludedFiles(data)
	require.NoError(t, err)
	require.Equal(t, []string{`a\sub\z 1.bin`, `a\x.bin`, `b\y.bin`}, got)

	got, err = DecodeExcludedFiles(nil)
	require.NoError(t, err)
	require.Empty(t, got)

	for _, bad := range []string{`..\x`, `a\..\..\x`, `\x`, `C:\x`, `a/x`, `a\\x`, `a\.\x`, `x:stream`, "a\x00b"} {
		_, err := DecodeExcludedFiles([]byte(bad + "\n"))
		require.Error(t, err, bad)
		require.False(t, IsVolumeRelativePath(bad), bad)
	}

	require.True(t, IsVolumeRelativePath(`Users\me\AppData\Local\kopia\0123456789abcdef\server-contents\ab\abcdef.f`))
}

func TestCreate_excludedFiles(t *testing.T) {
	ctx, env := repotesting.NewEnvironment(t, repotesting.FormatNotImportant)

	ntfsData := testVolumeData()
	opt := testCreateOptions(t, ntfsData, randomBytes(2, 3<<20))

	excluded := []string{`cache\b.bin`, `cache\a.bin`}

	for i := range opt.Volumes {
		if opt.Volumes[i].PartitionNumber == 3 {
			opt.Volumes[i].Excluded = []ExcludedFolder{{Path: "cache", Files: 2, Bytes: 8192}}
			opt.Volumes[i].ExcludedFiles = excluded
		}
	}

	man, md, err := Create(ctx, env.RepositoryWriter, opt)
	require.NoError(t, err)

	var ntfs, raw *Volume

	for i := range md.Volumes {
		switch md.Volumes[i].PartitionNumber {
		case 3:
			ntfs = &md.Volumes[i]
		case 1:
			raw = &md.Volumes[i]
		}
	}

	require.NotEmpty(t, ntfs.ExcludedFilesID)
	require.Empty(t, raw.ExcludedFilesID, "volumes without exclusions have no list")
	require.Equal(t, []ExcludedFolder{{Path: "cache", Files: 2, Bytes: 8192}}, ntfs.Excluded)

	got, err := ReadExcludedFiles(ctx, env.RepositoryWriter, ntfs)
	require.NoError(t, err)
	require.Equal(t, []string{`cache\a.bin`, `cache\b.bin`}, got)

	got, err = ReadExcludedFiles(ctx, env.RepositoryWriter, raw)
	require.NoError(t, err)
	require.Empty(t, got)

	// the list is an entry of the snapshot, so maintenance keeps it
	root, err := snapshotfs.SnapshotRoot(env.RepositoryWriter, man)
	require.NoError(t, err)

	dir, ok := root.(fs.Directory)
	require.True(t, ok)

	e, err := dir.Child(ctx, ExcludedEntryName(3, "C"))
	require.NoError(t, err)
	require.Equal(t, "p3-C.excluded", e.Name())
	require.Equal(t, "cache\\a.bin\ncache\\b.bin\n", string(readObject(ctx, t, e)))

	// the metadata read back from the snapshot has the list's ID
	md2, err := ReadMetadata(ctx, env.RepositoryWriter, man)
	require.NoError(t, err)

	for i := range md2.Volumes {
		if md2.Volumes[i].PartitionNumber == 3 {
			require.Equal(t, ntfs.ExcludedFilesID, md2.Volumes[i].ExcludedFilesID)
		}
	}
}

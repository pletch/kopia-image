package image

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/fs"
	"github.com/kopia/kopia/internal/repotesting"
	"github.com/kopia/kopia/repo/object"
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
	require.Equal(t, FormatVersionDeletesExcluded, md.FormatVersion, "the folder is not marked safe as zeros")
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

func TestRequiredFormatVersion(t *testing.T) {
	require.Equal(t, FormatVersion, RequiredFormatVersion(nil))
	require.Equal(t, FormatVersion, RequiredFormatVersion([]Volume{{}, {Excluded: []ExcludedFolder{{Path: "kopia", SafeAsZeros: true}}}}))
	require.Equal(t, FormatVersionDeletesExcluded, RequiredFormatVersion([]Volume{
		{Excluded: []ExcludedFolder{{Path: "kopia", SafeAsZeros: true}}},
		{Excluded: []ExcludedFolder{{Path: "Temp"}}},
	}))
}

func TestMetadataValidate_formatVersion(t *testing.T) {
	for v, ok := range map[int]bool{0: false, FormatVersion: true, FormatVersionDeletesExcluded: true, MaxFormatVersion + 1: false} {
		md := testMetadata(t)
		md.FormatVersion = v

		if ok {
			require.NoError(t, md.Validate(testLayout()), v)
		} else {
			require.ErrorContains(t, md.Validate(testLayout()), "unsupported image format version", v)
		}
	}
}

func TestWriteRestoreScript_excluded(t *testing.T) {
	md := testMetadata(t)
	md.FormatVersion = FormatVersionDeletesExcluded

	oid, err := object.ParseID("k0123456789abcdef0123456789abcdef")
	require.NoError(t, err)

	md.Volumes[1].ExcludedFilesID = oid
	md.Volumes[1].Excluded = []ExcludedFolder{{Path: "Temp", Files: 1, Bytes: 4096}}

	var sb strings.Builder

	require.NoError(t, WriteRestoreScript(&sb, testLayout(), md))

	script := sb.String()
	n := md.Volumes[1].PartitionNumber

	require.Contains(t, script, fmt.Sprintf("echo '%v: the files that the image left out, such as caches, hold zeros. Mount partition %v and delete them with:'", md.Volumes[1].EntryName, n))
	require.Contains(t, script, fmt.Sprintf("echo '  kopia image delete-excluded <snapshot ID of this image> %v <mount point>'", n))
	require.Contains(t, script, "echo 'or delete the files listed by: kopia show k0123456789abcdef0123456789abcdef'")
	require.NotContains(t, script, md.Volumes[0].EntryName+": the files")
}

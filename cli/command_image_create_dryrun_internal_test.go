package cli

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/snapshot/image"
)

func TestDryRunVolumeLines(t *testing.T) {
	v := &image.VolumeSource{
		PartitionNumber: 3, Label: "C", Size: 1000e9, FreeBytes: 400e9,
		UnstableExtents: []image.Extent{{Start: 0, Length: 1e9}},
	}

	require.Equal(t, []string{"  p3-C.img: 1 TB, 599 GB read, 401 GB zeroed, of which 0 B left out"}, dryRunVolumeLines(v))

	v.Excluded = []image.ExcludedFolder{
		{Source: "kopia cache", Path: `Users\me\AppData\Local\kopia\a\server-contents`, Recursive: true, Files: 3000, Bytes: 14e9, SafeAsZeros: true},
		{Source: `FilesNotToBackup\Temporary Files`, Path: `Users\me\AppData\Local\Temp`, Recursive: true, Files: 15000, Bytes: 13e9},
		{Source: `FilesNotToBackup\Kernel Dumps`, Path: `Windows`, Pattern: "memory.dmp", Files: 1, Bytes: 2e9},
		{Source: `common cache\Chrome`, Path: `Users\me\AppData\Local\Google\Chrome\User Data\Default\Cache`, Recursive: true, Files: 1700, Bytes: 4e8},
	}

	for i := range 10 {
		v.Excluded = append(v.Excluded, image.ExcludedFolder{Source: "kopia cache", Path: "small", Files: 1, Bytes: int64(i + 1)})
	}

	lines := dryRunVolumeLines(v)
	t.Log("\n" + strings.Join(lines, "\n"))

	require.Equal(t, "  p3-C.img: 1 TB, 569.6 GB read, 430.4 GB zeroed, of which 29.4 GB left out", lines[0])

	// sources, largest first
	require.Regexp(t, `^    FilesNotToBackup\s+15 GB  15,001 files in 2 folders$`, lines[1])
	require.Regexp(t, `^    kopia cache\s+14 GB  3,010 files in 11 folders$`, lines[2])
	require.Regexp(t, `^    common cache\s+400 MB  1,700 files in 1 folders$`, lines[3])

	// the largest folders, with their patterns and sources
	require.Equal(t, "    largest folders:", lines[4])
	require.Equal(t, `         14 GB  Users\me\AppData\Local\kopia\a\server-contents  (kopia cache)`, lines[5])
	require.Equal(t, `          2 GB  Windows\memory.dmp  (FilesNotToBackup\Kernel Dumps)`, lines[7])
	require.Equal(t, "    ... and 4 more folders", lines[15])

	// only kopia caches are safe as zeros; the rest needs a newer kopia
	require.Contains(t, lines[16], "A restore deletes these files")
	require.Len(t, lines, 17)

	v.Excluded = v.Excluded[:1]
	require.NotContains(t, strings.Join(dryRunVolumeLines(v), "\n"), "A restore deletes these files")
}

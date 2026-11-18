package ntfs_test

import (
	"crypto/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/snapshot/image"
	"github.com/kopia/kopia/snapshot/image/ntfs"
)

func writeRandomFile(t *testing.T, path string, n int) {
	t.Helper()

	b := make([]byte, n)
	_, err := rand.Read(b)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, b, 0o600))
}

func TestExclude(t *testing.T) {
	tmp := t.TempDir()
	cache := filepath.Join(tmp, "cache")
	outside := filepath.Join(tmp, "outside")

	require.NoError(t, os.MkdirAll(filepath.Join(cache, "sub"), 0o700))
	require.NoError(t, os.MkdirAll(outside, 0o700))

	writeRandomFile(t, filepath.Join(cache, "big.bin"), 1<<20)
	writeRandomFile(t, filepath.Join(cache, "sub", "nested.bin"), 300<<10)
	require.NoError(t, os.WriteFile(filepath.Join(cache, "tiny.txt"), []byte("resident"), 0o600))

	// a file with another name, and a junction: neither is excluded
	writeRandomFile(t, filepath.Join(cache, "linked.bin"), 200<<10)
	require.NoError(t, os.Link(filepath.Join(cache, "linked.bin"), filepath.Join(outside, "linked.bin")))

	writeRandomFile(t, filepath.Join(outside, "target.bin"), 200<<10)

	out, err := exec.CommandContext(t.Context(), "cmd", "/c", "mklink", "/J", filepath.Join(cache, "junction"), outside).CombinedOutput()
	require.NoError(t, err, string(out))

	root := filepath.VolumeName(tmp) + `\`
	rel := func(p string) string { return strings.TrimPrefix(p, root) }

	const clusterSize = 4096

	v := &ntfs.Volume{Data: ntfs.VolumeData{ClusterSize: clusterSize}}

	require.NoError(t, v.Exclude(root, []string{rel(cache), rel(filepath.Join(tmp, "missing"))}))

	require.Len(t, v.Excluded, 1)
	f := v.Excluded[0]
	require.Equal(t, rel(cache), f.Path)
	require.Equal(t, int64(3), f.Files, "big.bin, nested.bin and tiny.txt")
	require.Equal(t, image.TotalLength(v.ExcludedExtents), f.Bytes)

	extentsOf := func(name string) []image.Extent {
		e, err := ntfs.FileExtents(name, clusterSize)
		require.NoError(t, err)

		return e
	}

	big := extentsOf(filepath.Join(cache, "big.bin"))
	nested := extentsOf(filepath.Join(cache, "sub", "nested.bin"))

	require.Equal(t, image.TotalLength(big)+image.TotalLength(nested), f.Bytes, "the resident tiny.txt has no clusters")
	require.GreaterOrEqual(t, image.TotalLength(big), int64(1<<20))

	excluded, err := image.NormalizeExtents(v.ExcludedExtents)
	require.NoError(t, err)

	covered := func(e image.Extent) bool {
		for _, x := range excluded {
			if x.Start <= e.Start && e.End() <= x.End() {
				return true
			}
		}

		return false
	}

	for _, e := range append(big, nested...) {
		require.True(t, covered(e), "extent %+v not excluded", e)
	}

	for _, name := range []string{filepath.Join(cache, "linked.bin"), filepath.Join(outside, "target.bin")} {
		for _, e := range extentsOf(name) {
			require.False(t, covered(e), "%v must not be excluded", name)
		}
	}
}

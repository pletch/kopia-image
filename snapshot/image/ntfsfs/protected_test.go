package ntfsfs

import (
	"context"
	"maps"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/fs"
	"github.com/kopia/kopia/internal/repotesting"
)

// withProtectedPaths treats the given paths of the test image as protected.
func withProtectedPaths(t *testing.T, paths ...string) {
	t.Helper()

	saved := maps.Clone(protectedPaths)
	t.Cleanup(func() { protectedPaths = saved })

	protectedPaths = map[string]bool{}
	for _, p := range paths {
		protectedPaths[p] = true
	}
}

func openTestVolumeHiding(t *testing.T, hide bool) *Volume {
	t.Helper()

	ctx, env := repotesting.NewEnvironment(t, repotesting.FormatNotImportant)
	oid := writeImage(ctx, t, env.RepositoryWriter, testImage(t))

	v, err := Open(ctx, env.RepositoryWriter, oid, "p2-test", Options{})
	require.NoError(t, err)
	t.Cleanup(v.Close)

	// the tests may run as an administrator, who sees everything
	v.hideProtected = hide

	return v
}

func TestVolume_hidesProtectedPaths(t *testing.T) {
	withProtectedPaths(t, "folder a/folder b", "ones.bin")

	ctx := context.Background()
	root := openTestVolumeHiding(t, true).Root()

	rootNames := names(ctx, t, root)
	require.NotContains(t, rootNames, "ones.bin")
	require.Contains(t, rootNames, "Folder A")

	folderA, ok := lookup(ctx, t, root, "Folder A").(fs.Directory)
	require.True(t, ok)
	require.NotContains(t, names(ctx, t, folderA), "Folder B")

	// hidden entries cannot be reached by name either, in any case
	for _, name := range []string{"Folder B", "folder b", "FOLDER B"} {
		_, err := folderA.Child(ctx, name)
		require.ErrorIs(t, err, fs.ErrEntryNotFound, name)
	}

	_, err := root.Child(ctx, "ones.bin")
	require.ErrorIs(t, err, fs.ErrEntryNotFound)
}

// An alternate data stream "name:stream" is hidden with its file.
func TestVolume_hidesStreamsOfProtectedFiles(t *testing.T) {
	withProtectedPaths(t, "folder a/folder b/hello world text document.txt")

	ctx := context.Background()
	root := openTestVolumeHiding(t, true).Root()

	folderB, ok := lookup(ctx, t, root, "Folder A/Folder B").(fs.Directory)
	require.True(t, ok)

	for _, n := range names(ctx, t, folderB) {
		require.NotContains(t, n, "Hello world text document.txt")
	}
}

func TestVolume_showsProtectedPathsToAdministrators(t *testing.T) {
	withProtectedPaths(t, "folder a/folder b", "ones.bin")

	ctx := context.Background()
	root := openTestVolumeHiding(t, false).Root()

	require.Contains(t, names(ctx, t, root), "ones.bin")
	require.Equal(t, "Folder B", lookup(ctx, t, root, "Folder A/Folder B").Name())
}

func TestOpen_showProtectedOption(t *testing.T) {
	ctx, env := repotesting.NewEnvironment(t, repotesting.FormatNotImportant)
	oid := writeImage(ctx, t, env.RepositoryWriter, testImage(t))

	v, err := Open(ctx, env.RepositoryWriter, oid, "p2-test", Options{ShowProtected: true})
	require.NoError(t, err)
	t.Cleanup(v.Close)

	require.False(t, v.hideProtected)

	v2, err := Open(ctx, env.RepositoryWriter, oid, "p2-test", Options{})
	require.NoError(t, err)
	t.Cleanup(v2.Close)

	require.Equal(t, !processIsAdmin(), v2.hideProtected)
}

func TestProtectedPaths_coverWindowsCredentialStores(t *testing.T) {
	for _, p := range []string{
		"windows/system32/config",
		"windows/system32/microsoft/protect",
		"pagefile.sys",
		"hiberfil.sys",
	} {
		require.True(t, isProtectedPath(p), p)
	}

	require.False(t, isProtectedPath("windows/system32"))
	require.False(t, isProtectedPath("users"))
	require.Equal(t, "windows/system32/config", childPath(childPath(childPath("", "Windows"), "System32"), "CONFIG"))
}

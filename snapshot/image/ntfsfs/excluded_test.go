package ntfsfs

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/fs"
	"github.com/kopia/kopia/internal/repotesting"
)

func TestOpen_excludedFilesHidden(t *testing.T) {
	ctx, env := repotesting.NewEnvironment(t, repotesting.FormatNotImportant)
	oid := writeImage(ctx, t, env.RepositoryWriter, testImage(t))

	// excluded paths match in any case, like NTFS names
	v, err := Open(ctx, env.RepositoryWriter, oid, "p2-test", Options{Excluded: []string{
		`ONES.bin`,
		`Folder A\Folder B\Hello world text document.txt`,
	}})
	require.NoError(t, err)
	t.Cleanup(v.Close)

	root := v.Root()
	require.NotContains(t, names(ctx, t, root), "ones.bin")
	require.Contains(t, names(ctx, t, root), "Folder A")

	_, err = root.Child(ctx, "ones.bin")
	require.ErrorIs(t, err, fs.ErrEntryNotFound)

	// the file and its alternate data stream are both gone
	folderB := lookup(ctx, t, root, "Folder A/Folder B").(fs.Directory) //nolint:forcetypeassert
	require.Empty(t, names(ctx, t, folderB))

	// without the list, everything is there
	all, err := Open(ctx, env.RepositoryWriter, oid, "p2-test", Options{})
	require.NoError(t, err)
	t.Cleanup(all.Close)

	require.Contains(t, names(ctx, t, all.Root()), "ones.bin")
}

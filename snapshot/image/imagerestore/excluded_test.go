package imagerestore

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDeleteExcludedFiles(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()

	write := func(dir, name string) string {
		p := filepath.Join(dir, filepath.FromSlash(name))
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o700))
		require.NoError(t, os.WriteFile(p, make([]byte, 100), 0o600))

		return p
	}

	write(root, "cache/a.bin")
	write(root, "cache/sub/b.bin")
	readOnly := write(root, "cache/ro.bin")
	require.NoError(t, os.Chmod(readOnly, 0o400))

	keep := write(root, "cache/keep.bin")
	require.NoError(t, os.MkdirAll(filepath.Join(root, "cache", "dir.bin"), 0o700))

	// a link in place of a listed file is not followed
	target := write(outside, "target.bin")
	if err := os.Symlink(target, filepath.Join(root, "cache", "link.bin")); err != nil {
		t.Logf("no symbolic link: %v", err)
	}

	deleted, err := DeleteExcludedFiles(root, []string{
		`cache\a.bin`, `cache\sub\b.bin`, `cache\ro.bin`, `cache\missing.bin`, `cache\dir.bin`, `cache\link.bin`,
	})
	require.NoError(t, err)
	require.Equal(t, 3, deleted)

	for _, gone := range []string{"cache/a.bin", "cache/sub/b.bin", "cache/ro.bin"} {
		_, err := os.Lstat(filepath.Join(root, filepath.FromSlash(gone)))
		require.ErrorIs(t, err, os.ErrNotExist, gone)
	}

	for _, kept := range []string{keep, filepath.Join(root, "cache", "dir.bin"), target} {
		_, err := os.Stat(kept)
		require.NoError(t, err, kept)
	}

	// paths leaving the volume are refused before anything is deleted
	_, err = DeleteExcludedFiles(root, []string{`..\` + filepath.Base(outside) + `\target.bin`})
	require.ErrorContains(t, err, "invalid excluded file path")

	_, err = os.Stat(target)
	require.NoError(t, err)
}

func TestDeleteExcludedFiles_errors(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("uses a file held open without delete sharing")
	}

	root := t.TempDir()
	p := filepath.Join(root, "busy.bin")
	require.NoError(t, os.WriteFile(p, nil, 0o600))

	// Go opens files without FILE_SHARE_DELETE, so the file cannot be deleted
	f, err := os.Open(p)
	require.NoError(t, err)

	defer f.Close()

	deleted, err := DeleteExcludedFiles(root, []string{"busy.bin", "missing.bin"})
	require.Equal(t, 0, deleted)
	require.ErrorContains(t, err, "unable to delete 1 of 2 files")
}

func TestDeleteExcludedFiles_notThroughLinkedFolders(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()

	target := filepath.Join(outside, "cache", "a.bin")
	require.NoError(t, os.MkdirAll(filepath.Dir(target), 0o700))
	require.NoError(t, os.WriteFile(target, make([]byte, 100), 0o600))

	// a folder of the volume that links elsewhere, as "Documents and
	// Settings" does to C:\Users on a running Windows
	link := filepath.Join(root, "linked")

	if err := os.Symlink(outside, link); err != nil {
		if runtime.GOOS != "windows" {
			require.NoError(t, err)
		}

		// without the privilege for symbolic links, a junction
		if out, err := exec.CommandContext(t.Context(), "cmd", "/c", "mklink", "/J", link, outside).CombinedOutput(); err != nil {
			t.Skipf("unable to create a link: %v: %s", err, out)
		}
	}

	deleted, err := DeleteExcludedFiles(root, []string{`linked\cache\a.bin`})
	require.NoError(t, err)
	require.Zero(t, deleted)

	_, err = os.Stat(target)
	require.NoError(t, err, "the file behind the link is kept")
}

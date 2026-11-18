package localfs

import (
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/fs"
	"github.com/kopia/kopia/internal/testlogging"
)

// longAgo is far enough in the past that NTFS, which updates last access
// times at most hourly, updates it on any access.
var longAgo = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)

func lastAccess(t *testing.T, path string) time.Time {
	t.Helper()

	fi, err := os.Stat(path)
	require.NoError(t, err)

	attrs, ok := fi.Sys().(*syscall.Win32FileAttributeData)
	require.True(t, ok)

	return time.Unix(0, attrs.LastAccessTime.Nanoseconds())
}

func setLastAccess(t *testing.T, path string) {
	t.Helper()

	fi, err := os.Stat(path)
	require.NoError(t, err)
	require.NoError(t, os.Chtimes(path, longAgo, fi.ModTime()))
	require.True(t, lastAccess(t, path).Equal(longAgo))
}

func TestReadsPreserveLastAccessTime(t *testing.T) {
	ctx := testlogging.Context(t)
	dir := t.TempDir()
	sub := filepath.Join(dir, "sub")
	file := filepath.Join(sub, "file.txt")

	require.NoError(t, os.Mkdir(sub, 0o700))
	require.NoError(t, os.WriteFile(file, []byte("hello"), 0o600))

	// listing and reading with os.Open updates last access times, unless
	// NTFS was told not to keep them
	setLastAccess(t, sub)
	setLastAccess(t, file)

	entries, err := os.ReadDir(sub)
	require.NoError(t, err)
	require.Len(t, entries, 1)

	data, err := os.ReadFile(file)
	require.NoError(t, err)
	require.Equal(t, "hello", string(data))

	if lastAccess(t, sub).Equal(longAgo) && lastAccess(t, file).Equal(longAgo) {
		t.Skip("NTFS does not update last access times on this computer (fsutil behavior query disablelastaccess)")
	}

	// listing and reading through localfs does not
	setLastAccess(t, sub)
	setLastAccess(t, file)

	d, err := Directory(sub)
	require.NoError(t, err)

	all, err := fs.GetAllEntries(ctx, d)
	require.NoError(t, err)
	require.Len(t, all, 1)

	f, ok := all[0].(fs.File)
	require.True(t, ok)

	r, err := f.Open(ctx)
	require.NoError(t, err)

	data, err = io.ReadAll(r)
	require.NoError(t, err)
	require.NoError(t, r.Close())
	require.Equal(t, "hello", string(data))

	require.True(t, lastAccess(t, sub).Equal(longAgo), "the folder's last access time changed: %v", lastAccess(t, sub))
	require.True(t, lastAccess(t, file).Equal(longAgo), "the file's last access time changed: %v", lastAccess(t, file))
}

func TestOpenPreservingAccessTime_readOnlyFile(t *testing.T) {
	// a read-only file still opens, and is read
	file := filepath.Join(t.TempDir(), "ro.txt")
	require.NoError(t, os.WriteFile(file, []byte("data"), 0o400))

	f, err := openPreservingAccessTime(file, false)
	require.NoError(t, err)

	data, err := io.ReadAll(f)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	require.Equal(t, "data", string(data))
}

func TestOpenPreservingAccessTime_missing(t *testing.T) {
	_, err := openPreservingAccessTime(filepath.Join(t.TempDir(), "missing"), false)
	require.ErrorIs(t, err, os.ErrNotExist)

	_, err = openPreservingAccessTime(filepath.Join(t.TempDir(), "missing"), true)
	require.ErrorIs(t, err, os.ErrNotExist)
}

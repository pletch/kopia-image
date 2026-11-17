//go:build windows && !cgo

package winfspmount

import (
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/winfsp/cgofuse/fuse"

	"github.com/kopia/kopia/internal/mockfs"
)

func testFS(t *testing.T) (*kopiaFS, *atomic.Int32) {
	t.Helper()

	root := mockfs.NewDirectory()
	docs := root.AddDir("Docs", 0o755)
	docs.AddFile("Report.txt", []byte("0123456789"), 0o644)
	docs.AddSymlink("link", "Report.txt", 0o777)
	docs.AddDir("Sub", 0o755).AddFile("a.bin", make([]byte, 100_000), 0o644)

	var docsListed atomic.Int32

	docs.OnReaddir(func() { docsListed.Add(1) })

	return newKopiaFS(t.Context(), root), &docsListed
}

func TestGetattr(t *testing.T) {
	f, _ := testFS(t)

	var st fuse.Stat_t

	require.Equal(t, 0, f.Getattr("/", &st, ^uint64(0)))
	require.Equal(t, uint32(dirMode), st.Mode)

	require.Equal(t, 0, f.Getattr("/Docs/Report.txt", &st, ^uint64(0)))
	require.Equal(t, uint32(fileMode), st.Mode)
	require.EqualValues(t, 10, st.Size)

	// Windows names are case-insensitive
	require.Equal(t, 0, f.Getattr("/docs/REPORT.TXT", &st, ^uint64(0)))
	require.EqualValues(t, 10, st.Size)

	require.Equal(t, -fuse.ENOENT, f.Getattr("/Docs/missing", &st, ^uint64(0)))
	require.Equal(t, -fuse.ENOENT, f.Getattr("/Docs/link", &st, ^uint64(0)), "symlinks are not shown")
	require.Equal(t, -fuse.ENOTDIR, f.Getattr("/Docs/Report.txt/x", &st, ^uint64(0)))
}

func TestReaddir(t *testing.T) {
	f, _ := testFS(t)

	errc, _ := f.Opendir("/DOCS")
	require.Equal(t, 0, errc)

	got := map[string]uint32{}

	require.Equal(t, 0, f.Readdir("/DOCS", func(name string, st *fuse.Stat_t, _ int64) bool {
		if st != nil {
			got[name] = st.Mode
		} else {
			got[name] = 0
		}

		return true
	}, 0, ^uint64(0)))

	require.Equal(t, map[string]uint32{
		".":          0,
		"..":         0,
		"Report.txt": fileMode,
		"Sub":        dirMode,
	}, got)

	errc, _ = f.Opendir("/Docs/Report.txt")
	require.Equal(t, -fuse.ENOTDIR, errc)
}

// Every FUSE call names its file by path, so a folder must be listed once,
// not once for each of its entries.
func TestDirectoryIsListedOnce(t *testing.T) {
	f, docsListed := testFS(t)

	var st fuse.Stat_t

	for range 10 {
		require.Equal(t, 0, f.Getattr("/Docs/Report.txt", &st, ^uint64(0)))
		require.Equal(t, 0, f.Getattr("/docs/sub/a.bin", &st, ^uint64(0)))
		require.Equal(t, 0, f.Readdir("/Docs", func(string, *fuse.Stat_t, int64) bool { return true }, 0, ^uint64(0)))
	}

	require.EqualValues(t, 1, docsListed.Load())
}

func TestOpenAndRead(t *testing.T) {
	f, _ := testFS(t)

	errc, _ := f.Open("/Docs/Report.txt", fuse.O_RDWR)
	require.Equal(t, -fuse.EROFS, errc)

	errc, _ = f.Open("/Docs", fuse.O_RDONLY)
	require.Equal(t, -fuse.EISDIR, errc)

	errc, fh := f.Open("/Docs/Report.txt", fuse.O_RDONLY)
	require.Equal(t, 0, errc)

	buf := make([]byte, 4)

	require.Equal(t, 4, f.Read("", buf, 0, fh))
	require.Equal(t, "0123", string(buf))

	require.Equal(t, 4, f.Read("", buf, 4, fh)) // continues without seeking
	require.Equal(t, "4567", string(buf))

	require.Equal(t, 4, f.Read("", buf, 1, fh)) // seeks back
	require.Equal(t, "1234", string(buf))

	require.Equal(t, 2, f.Read("", buf, 8, fh)) // short read at the end
	require.Equal(t, "89", string(buf[:2]))

	require.Equal(t, 0, f.Read("", buf, 10, fh))

	require.Equal(t, 0, f.Release("", fh))
	require.Equal(t, -fuse.EBADF, f.Read("", buf, 0, fh))
}

func TestReadOnly(t *testing.T) {
	f, _ := testFS(t)

	errc, _ := f.Create("/new.txt", 0, 0o644)
	require.Equal(t, -fuse.EROFS, errc)
	require.Equal(t, -fuse.EROFS, f.Mkdir("/new", 0o755))
	require.Equal(t, -fuse.EROFS, f.Unlink("/Docs/Report.txt"))
	require.Equal(t, -fuse.EROFS, f.Rename("/Docs", "/Other"))
	require.Equal(t, -fuse.EROFS, f.Write("/Docs/Report.txt", []byte("x"), 0, 1))
}

// Only the user who mounted a snapshot can read it: the umask leaves the
// owner's read (and folder traversal) bits alone.
func TestMountOptions_ownerOnly(t *testing.T) {
	opts := strings.Join(mountOptions("x"), " ")
	require.Contains(t, opts, "uid=-1,gid=-1,umask=0277")
	require.Equal(t, uint32(0o500), uint32(dirMode&0o777)&^0o277)
	require.Equal(t, uint32(0o400), uint32(fileMode&0o777)&^0o277)
}

func TestVolumeLabel(t *testing.T) {
	require.Equal(t, "C on host, 2026-11-17", strings.ReplaceAll(volumeLabel("C on host, 2026-11-17"), "_", ","))
	require.Equal(t, "a_b_c", volumeLabel("a,b\\c"))
	require.Len(t, []rune(volumeLabel(strings.Repeat("x", 40))), maxLabelLength)
}

func TestFreeDriveLetter(t *testing.T) {
	d, err := freeDriveLetter()
	require.NoError(t, err)
	require.Len(t, d, 2)
	require.Equal(t, byte(':'), d[1])
}

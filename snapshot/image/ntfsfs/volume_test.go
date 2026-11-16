package ntfsfs

import (
	"bytes"
	"context"
	"crypto/sha1" //nolint:gosec // comparing with go-ntfs's own fixture
	"encoding/hex"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/fs"
	"github.com/kopia/kopia/internal/repotesting"
	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/object"
)

// testImage returns go-ntfs's NTFS test image, read from the module cache.
func testImage(t *testing.T) []byte {
	t.Helper()

	out, err := exec.CommandContext(t.Context(), "go", "list", "-m", "-f", "{{.Dir}}", "www.velocidex.com/golang/go-ntfs").Output()
	if err != nil {
		t.Skipf("go-ntfs module not found: %v", err)
	}

	b, err := os.ReadFile(filepath.Join(strings.TrimSpace(string(out)), "parser", "test_data", "test.ntfs.dd"))
	require.NoError(t, err)

	return b
}

func writeImage(ctx context.Context, t *testing.T, rep repo.RepositoryWriter, data []byte) object.ID {
	t.Helper()

	w := rep.NewObjectWriter(ctx, object.WriterOptions{Splitter: "FIXED-4M", Compressor: "zstd-fastest"})
	defer w.Close()

	_, err := w.Write(data)
	require.NoError(t, err)

	oid, err := w.Result()
	require.NoError(t, err)

	return oid
}

func openTestVolume(t *testing.T, cacheChunks int) *Volume {
	t.Helper()

	ctx, env := repotesting.NewEnvironment(t, repotesting.FormatNotImportant)
	oid := writeImage(ctx, t, env.RepositoryWriter, testImage(t))

	v, err := Open(ctx, env.RepositoryWriter, oid, "p2-test", Options{CacheChunks: cacheChunks})
	require.NoError(t, err)

	t.Cleanup(v.Close)

	return v
}

func lookup(ctx context.Context, t *testing.T, root fs.Directory, path string) fs.Entry {
	t.Helper()

	var e fs.Entry = root

	for part := range strings.SplitSeq(path, "/") {
		d, ok := e.(fs.Directory)
		require.True(t, ok, "%v is not a directory", e.Name())

		var err error

		e, err = d.Child(ctx, part)
		require.NoError(t, err, path)
	}

	return e
}

func readFile(ctx context.Context, t *testing.T, e fs.Entry) []byte {
	t.Helper()

	f, ok := e.(fs.File)
	require.True(t, ok, "%v is not a file", e.Name())

	r, err := f.Open(ctx)
	require.NoError(t, err)

	defer r.Close()

	b, err := io.ReadAll(r)
	require.NoError(t, err)

	return b
}

func names(ctx context.Context, t *testing.T, d fs.Directory) []string {
	t.Helper()

	entries, err := fs.GetAllEntries(ctx, d)
	require.NoError(t, err)

	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}

	return out
}

func TestVolume_browse(t *testing.T) {
	ctx := context.Background()
	v := openTestVolume(t, 0)
	root := v.Root()

	require.Equal(t, "p2-test", root.Name())

	rootNames := names(ctx, t, root)
	t.Logf("root: %v", rootNames)
	require.Contains(t, rootNames, "Folder A")
	require.Contains(t, rootNames, "ones.bin")

	// NTFS metadata files are hidden; $RECYCLE.BIN is an ordinary folder
	for _, meta := range []string{"$MFT", "$MFTMirr", "$LogFile", "$Volume", "$AttrDef", ".", "$Bitmap", "$Boot", "$BadClus", "$Secure", "$UpCase", "$Extend"} {
		require.NotContains(t, rootNames, meta)
	}

	require.Contains(t, rootNames, "$RECYCLE.BIN")

	folderB := lookup(ctx, t, root, "Folder A/Folder B")
	require.True(t, folderB.IsDir())
	folderBDir, ok := folderB.(fs.Directory)
	require.True(t, ok)
	require.Contains(t, names(ctx, t, folderBDir), "Hello world text document.txt:goodbye.txt")

	hello := lookup(ctx, t, root, "Folder A/Folder B/Hello world text document.txt")
	require.False(t, hello.IsDir())
	require.Equal(t, int64(12), hello.Size())
	require.Equal(t, "Hello world!", string(readFile(ctx, t, hello)))
	require.False(t, hello.ModTime().IsZero())

	ads := lookup(ctx, t, root, "Folder A/Folder B/Hello world text document.txt:goodbye.txt")
	require.Equal(t, "Goodbye cruel world.", string(readFile(ctx, t, ads)))

	// NTFS-compressed, in many runs
	ones := readFile(ctx, t, lookup(ctx, t, root, "ones.bin"))
	require.Len(t, ones, 2949120)

	sum := sha1.Sum(ones) //nolint:gosec
	require.Equal(t, "f581eebdb9a49a622c305d4e44977bc91a4a1204", hex.EncodeToString(sum[:]))

	// names are case-insensitive
	require.Equal(t, "Folder A", lookup(ctx, t, root, "folder a").Name())

	_, err := root.Child(ctx, "no such file")
	require.ErrorIs(t, err, fs.ErrEntryNotFound)
}

func TestVolume_seekAndPartialReads(t *testing.T) {
	ctx := context.Background()
	v := openTestVolume(t, 0)

	f := lookup(ctx, t, v.Root(), "ones.bin").(fs.File) //nolint:forcetypeassert
	whole := readFile(ctx, t, f)

	r, err := f.Open(ctx)
	require.NoError(t, err)

	defer r.Close()

	_, err = r.Seek(100000, io.SeekStart)
	require.NoError(t, err)

	part := make([]byte, 70000)
	_, err = io.ReadFull(r, part)
	require.NoError(t, err)
	require.True(t, bytes.Equal(whole[100000:170000], part))
}

func TestVolume_tinyCacheStillWorks(t *testing.T) {
	ctx := context.Background()

	// a one-chunk cache forces chunks to be evicted and fetched again
	v := openTestVolume(t, 1)

	require.Equal(t, "Hello world!", string(readFile(ctx, t, lookup(ctx, t, v.Root(), "Folder A/Folder B/Hello world text document.txt"))))
	require.Len(t, readFile(ctx, t, lookup(ctx, t, v.Root(), "ones.bin")), 2949120)
	require.Positive(t, v.Fetches())
}

func TestOpen_notNTFS(t *testing.T) {
	ctx, env := repotesting.NewEnvironment(t, repotesting.FormatNotImportant)
	oid := writeImage(ctx, t, env.RepositoryWriter, make([]byte, 8<<20))

	_, err := Open(ctx, env.RepositoryWriter, oid, "x", Options{})
	require.ErrorIs(t, err, ErrNotNTFS)
}

func TestVolume_skipStreams(t *testing.T) {
	ctx, env := repotesting.NewEnvironment(t, repotesting.FormatNotImportant)
	oid := writeImage(ctx, t, env.RepositoryWriter, testImage(t))

	v, err := Open(ctx, env.RepositoryWriter, oid, "p2-test", Options{SkipStreams: true})
	require.NoError(t, err)

	defer v.Close()

	folderB := lookup(ctx, t, v.Root(), "Folder A/Folder B").(fs.Directory) //nolint:forcetypeassert
	require.Equal(t, []string{"Hello world text document.txt"}, names(ctx, t, folderB))
}

package cli_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/internal/vhdxtest"
	"github.com/kopia/kopia/snapshot/image/blockdev"
	"github.com/kopia/kopia/snapshot/image/gpt"
	"github.com/kopia/kopia/tests/testenv"
)

// readGPTSectors returns the primary GPT header and partition entries of a
// disk: LBA 1-33 with 512-byte sectors, LBA 1-5 with 4096-byte sectors.
func readGPTSectors(t *testing.T, diskNumber uint32, sectorSize int) []byte {
	t.Helper()

	dev, err := blockdev.Open(blockdev.DiskPath(diskNumber))
	require.NoError(t, err)

	defer dev.Close()

	const entryArrayBytes = 128 * 128

	b := make([]byte, sectorSize+entryArrayBytes)
	_, err = dev.ReadAt(b, int64(sectorSize))
	require.NoError(t, err)

	return b
}

func TestImageRestoreToDiskAndFile(t *testing.T) {
	src := vhdxtest.New(t)
	mnt := src.FormatAndMount()

	want := map[string]string{}
	for i := range 50 {
		name := fmt.Sprintf("dir%d/file%02d.txt", i%5, i)
		want[name] = strings.Repeat(fmt.Sprintf("content %v\n", i), i*100+1)
		require.NoError(t, os.MkdirAll(filepath.Join(mnt, filepath.Dir(name)), 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(mnt, name), []byte(want[name]), 0o600))
	}

	env := testenv.NewCLITest(t, testenv.RepoFormatNotImportant, testenv.NewInProcRunner(t))
	env.RunAndExpectSuccess(t, "repo", "create", "filesystem", "--path", env.RepoDir)

	env.RunAndExpectSuccess(t, "image", "create", mnt, "--state-file="+filepath.Join(t.TempDir(), "state.json"))
	source := env.RunAndExpectSuccess(t, "image", "list")[0]

	// restore to a raw disk image file, which is created
	imageFile := filepath.Join(t.TempDir(), "disk.img")
	out := env.RunAndExpectSuccess(t, "image", "restore", source, "--target", imageFile)
	t.Logf("restore to file:\n%s", strings.Join(out, "\n"))

	f, err := os.Open(imageFile)
	require.NoError(t, err)

	table, err := gpt.Read(f, 512)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	require.Len(t, table.Partitions, 2, "MSR and data partition")

	// restore to a blank disk: refused while the source disk, with the same disk GUID, is attached
	dst := vhdxtest.NewBlank(t)
	target := blockdev.DiskPath(dst.Number())

	env.RunAndExpectSuccess(t, "image", "restore", source, "--target", target, "--dry-run", "--force")

	_, stderr := env.RunAndExpectFailure(t, "image", "restore", source, "--target", target)
	require.Contains(t, strings.Join(stderr, "\n"), "same disk GUID")

	out = env.RunAndExpectSuccess(t, "image", "restore", source, "--target", target, "--force", "--parallel", "8")
	t.Logf("restore to disk:\n%s", strings.Join(out, "\n"))

	// the restored partition table is byte-identical to the one Windows created
	require.Equal(t, readGPTSectors(t, src.Number(), 512), readGPTSectors(t, dst.Number(), 512))

	// a second restore is refused: the disk now has a partition table, and once
	// Windows recognizes the restored volume, a file system too
	_, stderr = env.RunAndExpectFailure(t, "image", "restore", source, "--target", target, "--force")
	refusal := strings.Join(stderr, "\n")
	t.Logf("second restore: %s", refusal)
	require.True(t, strings.Contains(refusal, "--overwrite") || strings.Contains(refusal, "remove its partitions first"), refusal)

	// with the source gone, Windows mounts the restored volume
	src.Detach()
	dst.Reattach()

	restored := dst.Mount()

	for name, data := range want {
		got, err := os.ReadFile(filepath.Join(restored, name))
		require.NoError(t, err, name)
		require.Equal(t, data, string(got), name)
	}

	chk, err := exec.CommandContext(t.Context(), "chkdsk", restored, "/scan").CombinedOutput()
	require.NoError(t, err, "chkdsk:\n%s", chk)
	require.Contains(t, string(chk), "found no problems")

	// --only restores the data volume in place while it is mounted: its volume
	// is locked and dismounted, and Windows mounts it again afterwards
	require.NoError(t, os.WriteFile(filepath.Join(restored, "dir0", "file00.txt"), []byte("changed"), 0o600))

	// reattaching it may have changed its number
	target = blockdev.DiskPath(dst.Number())

	out = env.RunAndExpectSuccess(t, "image", "restore", source, "--target", target, "--only", "2")
	t.Logf("partial restore to disk:\n%s", strings.Join(out, "\n"))

	restored = dst.Mount()

	for name, data := range want {
		got, err := os.ReadFile(filepath.Join(restored, name))
		require.NoError(t, err, name)
		require.Equal(t, data, string(got), name)
	}

	chk, err = exec.CommandContext(t.Context(), "chkdsk", restored, "/scan").CombinedOutput()
	require.NoError(t, err, "chkdsk after the partial restore:\n%s", chk)
	require.Contains(t, string(chk), "found no problems")
}

// TestImageRestore4KSectors images a disk with 4096-byte sectors, whose GPT
// reserves 6 sectors at the start and 5 at the end rather than 34 and 33, and
// restores it to a raw file and to a blank 4K disk. A 512-byte disk is refused.
func TestImageRestore4KSectors(t *testing.T) {
	src := vhdxtest.New4K(t)
	mnt := src.FormatAndMount()

	srcLayout, err := blockdev.ReadLayout(src.Number())
	require.NoError(t, err)
	require.Equal(t, int64(4096), srcLayout.SectorSize)

	want := map[string]string{}
	for i := range 50 {
		name := fmt.Sprintf("dir%d/file%02d.txt", i%5, i)
		want[name] = strings.Repeat(fmt.Sprintf("content %v\n", i), i*100+1)
		require.NoError(t, os.MkdirAll(filepath.Join(mnt, filepath.Dir(name)), 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(mnt, name), []byte(want[name]), 0o600))
	}

	env := testenv.NewCLITest(t, testenv.RepoFormatNotImportant, testenv.NewInProcRunner(t))
	env.RunAndExpectSuccess(t, "repo", "create", "filesystem", "--path", env.RepoDir)

	out := env.RunAndExpectSuccess(t, "image", "create", mnt, "--state-file="+filepath.Join(t.TempDir(), "state.json"))
	t.Logf("create:\n%s", strings.Join(out, "\n"))

	source := env.RunAndExpectSuccess(t, "image", "list")[0]

	// a raw disk image file gets the image's 4096-byte sectors
	imageFile := filepath.Join(t.TempDir(), "disk.img")
	out = env.RunAndExpectSuccess(t, "image", "restore", source, "--target", imageFile)
	t.Logf("restore to file:\n%s", strings.Join(out, "\n"))

	f, err := os.Open(imageFile)
	require.NoError(t, err)

	table, err := gpt.Read(f, 4096)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	require.Len(t, table.Partitions, 2, "MSR and data partition")

	// a disk with 512-byte sectors cannot hold the image
	small := vhdxtest.NewBlank(t)
	_, stderr := env.RunAndExpectFailure(t, "image", "restore", source, "--target", blockdev.DiskPath(small.Number()), "--force")
	require.Contains(t, strings.Join(stderr, "\n"), "4096-byte sectors")

	dst := vhdxtest.NewBlank4K(t)
	target := blockdev.DiskPath(dst.Number())

	out = env.RunAndExpectSuccess(t, "image", "restore", source, "--target", target, "--force", "--parallel", "8")
	t.Logf("restore to disk:\n%s", strings.Join(out, "\n"))

	// the restored partition table is byte-identical to the one Windows created
	require.Equal(t, readGPTSectors(t, src.Number(), 4096), readGPTSectors(t, dst.Number(), 4096))

	dstLayout, err := blockdev.ReadLayout(dst.Number())
	require.NoError(t, err)
	require.Equal(t, srcLayout.Partitions, dstLayout.Partitions)

	// with the source gone, Windows mounts the restored volume
	src.Detach()
	dst.Reattach()

	restored := dst.Mount()

	for name, data := range want {
		got, err := os.ReadFile(filepath.Join(restored, name))
		require.NoError(t, err, name)
		require.Equal(t, data, string(got), name)
	}

	chk, err := exec.CommandContext(t.Context(), "chkdsk", restored, "/scan").CombinedOutput()
	require.NoError(t, err, "chkdsk:\n%s", chk)
	require.Contains(t, string(chk), "found no problems")
}

// TestImageRestoreDeletesExcludedFiles images a volume holding a kopia cache:
// the cache's files are left out of listings, and restoring the image to a
// disk deletes them from the restored volume, which Windows mounts by itself.
func TestImageRestoreDeletesExcludedFiles(t *testing.T) {
	src := vhdxtest.New(t)
	mnt := src.FormatAndMount()

	cacheRel := filepath.Join("Users", "someone", "AppData", "Local", "kopia", "0123456789abcdef")
	files := map[string]string{
		filepath.Join(cacheRel, "server-contents", "ab", "abcdef.f"): strings.Repeat("cached ", 100_000),
		filepath.Join(cacheRel, "indexes", "x1.sndx"):                strings.Repeat("index ", 10_000),
		"other.txt": strings.Repeat("other ", 10_000),
	}

	for name, data := range files {
		require.NoError(t, os.MkdirAll(filepath.Dir(filepath.Join(mnt, name)), 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(mnt, name), []byte(data), 0o600))
	}

	env := testenv.NewCLITest(t, testenv.RepoFormatNotImportant, testenv.NewInProcRunner(t))
	env.RunAndExpectSuccess(t, "repo", "create", "filesystem", "--path", env.RepoDir)

	env.RunAndExpectSuccess(t, "image", "create", mnt, "--state-file="+filepath.Join(t.TempDir(), "state.json"))
	source := env.RunAndExpectSuccess(t, "image", "list")[0]

	// browsing leaves the cache file out, but not the index cache
	cacheInImage := source + "/2/" + filepath.ToSlash(cacheRel)
	require.Empty(t, env.RunAndExpectSuccess(t, "image", "ls", cacheInImage+"/server-contents/ab"))
	require.Equal(t, []string{"x1.sndx"}, env.RunAndExpectSuccess(t, "image", "ls", cacheInImage+"/indexes"))

	// with the source detached, nothing keeps the restored disk offline
	src.Detach()

	dst := vhdxtest.NewBlank(t)
	out := env.RunAndExpectSuccess(t, "image", "restore", source, "--target", blockdev.DiskPath(dst.Number()))
	t.Logf("restore:\n%s", strings.Join(out, "\n"))
	require.Contains(t, strings.Join(out, "\n"), "deleted 1 of the 1 files that the image left out")

	restored := dst.Mount()

	_, err := os.Stat(filepath.Join(restored, cacheRel, "server-contents", "ab", "abcdef.f"))
	require.ErrorIs(t, err, os.ErrNotExist, "the cache file is deleted")

	for _, name := range []string{filepath.Join(cacheRel, "indexes", "x1.sndx"), "other.txt"} {
		got, err := os.ReadFile(filepath.Join(restored, name))
		require.NoError(t, err, name)
		require.Equal(t, files[name], string(got), name)
	}

	// delete-excluded deletes them from a copy restored by other means
	stale := filepath.Join(restored, cacheRel, "server-contents", "ab", "abcdef.f")
	require.NoError(t, os.WriteFile(stale, make([]byte, 4096), 0o600))

	out = env.RunAndExpectSuccess(t, "image", "delete-excluded", source, "2", restored)
	require.Contains(t, strings.Join(out, "\n"), "deleted 1 of the 1 files")

	_, err = os.Stat(stale)
	require.ErrorIs(t, err, os.ErrNotExist)

	chk, err := exec.CommandContext(t.Context(), "chkdsk", restored, "/scan").CombinedOutput()
	require.NoError(t, err, "chkdsk:\n%s", chk)
	require.Contains(t, string(chk), "found no problems")
}

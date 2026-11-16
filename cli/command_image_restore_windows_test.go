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

// readGPTSectors returns LBA 1-33 of a 512-byte-sector disk: the primary
// GPT header and partition entries.
func readGPTSectors(t *testing.T, diskNumber uint32) []byte {
	t.Helper()

	dev, err := blockdev.Open(blockdev.DiskPath(diskNumber))
	require.NoError(t, err)

	defer dev.Close()

	b := make([]byte, 33*512)
	_, err = dev.ReadAt(b, 512)
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
	require.Equal(t, readGPTSectors(t, src.Number()), readGPTSectors(t, dst.Number()))

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
}

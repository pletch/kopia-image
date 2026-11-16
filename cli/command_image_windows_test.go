package cli_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/internal/vhdxtest"
	"github.com/kopia/kopia/tests/testenv"
)

func TestImageCreateAndList(t *testing.T) {
	mnt := vhdxtest.New(t).FormatAndMount()
	require.NoError(t, os.WriteFile(filepath.Join(mnt, "hello.txt"), []byte("hello"), 0o600))

	env := testenv.NewCLITest(t, testenv.RepoFormatNotImportant, testenv.NewInProcRunner(t))
	env.RunAndExpectSuccess(t, "repo", "create", "filesystem", "--path", env.RepoDir)

	stateFile := "--state-file=" + filepath.Join(t.TempDir(), "state.json")

	out := env.RunAndExpectSuccess(t, "image", "create", mnt, "--dry-run", stateFile)
	require.Contains(t, strings.Join(out, "\n"), "p2-data.img (NTFS from a shadow copy of")
	require.Contains(t, strings.Join(out, "\n"), "Dry run of disk")

	_, stderr := env.RunAndExpectSuccessWithErrOut(t, "image", "list")
	require.Contains(t, strings.Join(stderr, "\n"), "No image snapshots found")

	out = env.RunAndExpectSuccess(t, "image", "create", mnt, stateFile)
	require.Contains(t, strings.Join(out, "\n"), "Created image snapshot")

	out = env.RunAndExpectSuccess(t, "image", "list")
	require.Len(t, out, 2, "source header and one image: %v", out)
	require.Contains(t, out[0], ":image:disk:")
	require.Contains(t, out[1], "zero chunks")

	source := out[0]

	out = env.RunAndExpectSuccess(t, "image", "check")
	require.Contains(t, strings.Join(out, "\n"), "All 1 image snapshots passed.")
	out = env.RunAndExpectSuccess(t, "image", "check", source, "--full", "--ntfs")
	require.Contains(t, strings.Join(out, "\n"), "p2-data.img file system: OK")

	// image sources work with the standard snapshot and policy commands
	require.Len(t, env.RunAndExpectSuccess(t, "snapshot", "list", source), 2)
	env.RunAndExpectSuccess(t, "policy", "set", source, "--keep-latest=1", "--keep-hourly=0", "--keep-daily=0",
		"--keep-weekly=0", "--keep-monthly=0", "--keep-annual=0")

	env.RunAndExpectSuccess(t, "image", "create", mnt, stateFile)

	out = env.RunAndExpectSuccess(t, "image", "list")
	require.Len(t, out, 2, "retention keeps only the latest image: %v", out)

	// non-image paths and unsupported inputs fail cleanly
	env.RunAndExpectFailure(t, "image", "create", filepath.Join(mnt, "no-such-dir"), stateFile)
}

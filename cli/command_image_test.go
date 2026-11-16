package cli_test

import (
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/tests/testenv"
)

func TestSnapshotCreateRejectsImageSource(t *testing.T) {
	env := testenv.NewCLITest(t, testenv.RepoFormatNotImportant, testenv.NewInProcRunner(t))
	env.RunAndExpectSuccess(t, "repo", "create", "filesystem", "--path", env.RepoDir)

	_, stderr := env.RunAndExpectFailure(t, "snapshot", "create", "image:disk:0f6ab3f2-1c3d-4e5f-8a9b-0c1d2e3f4a5b")
	require.Contains(t, strings.Join(stderr, "\n"), "kopia image create")
}

func TestImageCreateUnsupportedPlatform(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("image create is supported on Windows")
	}

	env := testenv.NewCLITest(t, testenv.RepoFormatNotImportant, testenv.NewInProcRunner(t))
	env.RunAndExpectSuccess(t, "repo", "create", "filesystem", "--path", env.RepoDir)

	_, stderr := env.RunAndExpectFailure(t, "image", "create", "/")
	require.Contains(t, strings.Join(stderr, "\n"), "supported only on Windows")

	_, stderr = env.RunAndExpectSuccessWithErrOut(t, "image", "list")
	require.Contains(t, strings.Join(stderr, "\n"), "No image snapshots found")
}

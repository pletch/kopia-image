package cli_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/internal/clock"
	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/snapshot/image/recoverymedia"
	"github.com/kopia/kopia/tests/testenv"
)

// newSharedMediaTest returns a test environment with a repository and a
// folder standing in for media built on another computer, whose record
// leads to repository.
func newSharedMediaTest(t *testing.T, builtOn, repository string) (env *testenv.CLITest, media, recordFile string) {
	t.Helper()

	if runtime.GOOS != "windows" {
		t.Skip("recovery media is supported on Windows only")
	}

	env = testenv.NewCLITest(t, testenv.RepoFormatNotImportant, testenv.NewInProcRunner(t))
	env.RunAndExpectSuccess(t, "repo", "create", "filesystem", "--path", env.RepoDir)

	if repository == "" {
		repository = thisRepository(t, env)
	}

	media = t.TempDir()
	require.NoError(t, recoverymedia.WriteRecord(filepath.Join(media, recoverymedia.MediaRecordFile), &recoverymedia.Record{
		BuiltAt:          clock.Now().UTC().Add(-time.Hour),
		KopiaVersion:     repo.BuildVersion,
		WinREDigest:      "builder's",
		ConnectionDigest: "builder's",
		BuiltOn:          builtOn,
		Repository:       repository,
	}))

	return env, media, filepath.Join(t.TempDir(), recoverymedia.RecordFileName)
}

func thisRepository(t *testing.T, env *testenv.CLITest) string {
	t.Helper()

	lc, err := repo.LoadConfigFromFile(filepath.Join(env.ConfigDir, ".kopia.config"))
	require.NoError(t, err)

	return recoverymedia.DescribeRepository(lc)
}

func TestImageRecoveryMediaRegister(t *testing.T) {
	env, media, recordFile := newSharedMediaTest(t, "OTHER-PC", "")

	out := strings.Join(env.RunAndExpectSuccess(t, "image", "recovery-media", "register", media, "--record-file", recordFile), "\n")
	require.Contains(t, out, "Registered the recovery media built on OTHER-PC")
	require.Contains(t, out, "It has the storage and network drivers of OTHER-PC")

	r, err := recoverymedia.ReadRecord(recordFile)
	require.NoError(t, err)
	require.True(t, r.Shared())
	require.Equal(t, "OTHER-PC", r.BuiltOn)
	require.Equal(t, media, r.Target)
	require.Equal(t, thisRepository(t, env), r.Repository)

	// status checks the registered media instead of reporting none
	out = strings.Join(env.RunAndExpectSuccess(t, "image", "recovery-media", "status", "--record-file", recordFile), "\n")
	require.Contains(t, out, "Recovery media built on OTHER-PC")
	require.Contains(t, out, "registered from "+media)
	require.Contains(t, out, "It matches this computer.")
}

func TestImageRecoveryMediaRegister_acceptsTheRecordFile(t *testing.T) {
	env, media, recordFile := newSharedMediaTest(t, "OTHER-PC", "")

	env.RunAndExpectSuccess(t, "image", "recovery-media", "register", filepath.Join(media, recoverymedia.MediaRecordFile), "--record-file", recordFile)
	require.FileExists(t, recordFile)
}

func TestImageRecoveryMediaRegister_otherRepository(t *testing.T) {
	env, media, recordFile := newSharedMediaTest(t, "OTHER-PC", "S3 bucket elsewhere at s3.example.com")

	_, stderr := env.RunAndExpectFailure(t, "image", "recovery-media", "register", media, "--record-file", recordFile)
	require.Contains(t, strings.Join(stderr, "\n"), "the media leads to S3 bucket elsewhere at s3.example.com")
	require.NoFileExists(t, recordFile)

	// --force registers it, compared from now on with this computer's connection
	env.RunAndExpectSuccess(t, "image", "recovery-media", "register", media, "--force", "--record-file", recordFile)

	r, err := recoverymedia.ReadRecord(recordFile)
	require.NoError(t, err)
	require.Equal(t, thisRepository(t, env), r.Repository)
}

func TestImageRecoveryMediaRegister_builtHere(t *testing.T) {
	host, err := os.Hostname()
	require.NoError(t, err)

	env, media, recordFile := newSharedMediaTest(t, strings.ToLower(host), "")

	_, stderr := env.RunAndExpectFailure(t, "image", "recovery-media", "register", media, "--record-file", recordFile)
	require.Contains(t, strings.Join(stderr, "\n"), "built on this computer")
	require.NoFileExists(t, recordFile)
}

func TestImageRecoveryMediaRegister_mediaWithoutRecord(t *testing.T) {
	env, _, recordFile := newSharedMediaTest(t, "OTHER-PC", "")
	empty := t.TempDir()

	_, stderr := env.RunAndExpectFailure(t, "image", "recovery-media", "register", empty, "--record-file", recordFile)
	require.Contains(t, strings.Join(stderr, "\n"), "has no "+recoverymedia.MediaRecordFile)
	require.NoFileExists(t, recordFile)
}

func TestImageRecoveryMediaStatus_none(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("recovery media is supported on Windows only")
	}

	env := testenv.NewCLITest(t, testenv.RepoFormatNotImportant, testenv.NewInProcRunner(t))
	env.RunAndExpectSuccess(t, "repo", "create", "filesystem", "--path", env.RepoDir)

	out := strings.Join(env.RunAndExpectSuccess(t, "image", "recovery-media", "status", "--record-file", filepath.Join(t.TempDir(), "none.json")), "\n")
	require.Contains(t, out, "No recovery media was built on this computer")
	require.Contains(t, out, "kopia image recovery-media register")
}

func TestImageDrivers_none(t *testing.T) {
	env := testenv.NewCLITest(t, testenv.RepoFormatNotImportant, testenv.NewInProcRunner(t))
	env.RunAndExpectSuccess(t, "repo", "create", "filesystem", "--path", env.RepoDir)

	_, stderr := env.RunAndExpectSuccessWithErrOut(t, "image", "drivers")
	require.Contains(t, strings.Join(stderr, "\n"), "No image stores drivers yet")

	out := env.RunAndExpectSuccess(t, "image", "drivers", "--json")
	require.JSONEq(t, "[]", strings.Join(out, "\n"))
}

package endtoend_test

import (
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/cli"
	"github.com/kopia/kopia/internal/testutil"
	"github.com/kopia/kopia/snapshot"
	"github.com/kopia/kopia/tests/testenv"
)

// writeUniqueRandomFile writes a file that neither compresses nor deduplicates.
func writeUniqueRandomFile(t *testing.T, path string, size int) {
	t.Helper()

	b := make([]byte, size)

	_, err := rand.Read(b)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, b, 0o600))
}

// TestSnapshotCreate_recordsUploadedBytes checks that each snapshot records
// what it uploaded: its own new data, also when one command snapshots
// several sources in one write session, and almost nothing when nothing
// changed.
func TestSnapshotCreate_recordsUploadedBytes(t *testing.T) {
	t.Parallel()

	const (
		sizeA = 1 << 20
		sizeB = 3 << 20

		// packs carry headers, directory manifests and padding
		overhead = 256 << 10
	)

	runner := testenv.NewInProcRunner(t)
	e := testenv.NewCLITest(t, testenv.RepoFormatNotImportant, runner)

	defer e.RunAndExpectSuccess(t, "repo", "disconnect")

	e.RunAndExpectSuccess(t, "repo", "create", "filesystem", "--path", e.RepoDir)

	dirA := testutil.TempDirectory(t)
	dirB := testutil.TempDirectory(t)

	writeUniqueRandomFile(t, filepath.Join(dirA, "a"), sizeA)
	writeUniqueRandomFile(t, filepath.Join(dirB, "b"), sizeB)

	e.RunAndExpectSuccess(t, "snapshot", "create", dirA, dirB)

	uploaded := func(dir string) []*int64 {
		var mans []cli.SnapshotManifest

		testutil.MustParseJSONLines(t, e.RunAndExpectSuccess(t, "snapshot", "list", dir, "--json"), &mans)

		var out []*int64
		for _, m := range mans {
			out = append(out, m.UploadedBytes)
		}

		return out
	}

	a, b := uploaded(dirA), uploaded(dirB)
	require.Len(t, a, 1)
	require.Len(t, b, 1)
	require.NotNil(t, a[0])
	require.NotNil(t, b[0])

	require.GreaterOrEqual(t, *a[0], int64(sizeA))
	require.Less(t, *a[0], int64(sizeA+overhead))
	require.GreaterOrEqual(t, *b[0], int64(sizeB))
	require.Less(t, *b[0], int64(sizeB+overhead))

	// nothing changed: only the directory, which deduplicates too
	var again snapshot.Manifest

	testutil.MustParseJSONLines(t, e.RunAndExpectSuccess(t, "snapshot", "create", dirA, "--json"), &again)
	require.NotNil(t, again.UploadedBytes)
	require.Less(t, *again.UploadedBytes, int64(overhead))
	require.Equal(t, again.UploadedBytes, uploaded(dirA)[1])
}

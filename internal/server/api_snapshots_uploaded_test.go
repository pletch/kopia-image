package server_test

import (
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/internal/apiclient"
	"github.com/kopia/kopia/internal/repotesting"
	"github.com/kopia/kopia/internal/serverapi"
	"github.com/kopia/kopia/internal/servertesting"
	"github.com/kopia/kopia/internal/testutil"
	"github.com/kopia/kopia/snapshot"
	"github.com/kopia/kopia/snapshot/policy"
)

// TestSnapshotRecordsUploadedBytes checks that a snapshot the server takes
// records its new data, and that the sources list and the snapshot history
// report it.
func TestSnapshotRecordsUploadedBytes(t *testing.T) {
	const (
		size = 1 << 20

		// directory manifests and per-content overhead
		overhead = 64 << 10
	)

	ctx, env := repotesting.NewEnvironment(t, repotesting.FormatNotImportant)
	srvInfo := servertesting.StartServer(t, env, false)

	cli, err := apiclient.NewKopiaAPIClient(apiclient.Options{
		BaseURL:  srvInfo.BaseURL,
		Username: servertesting.TestUIUsername,
		Password: servertesting.TestUIPassword,
	})
	require.NoError(t, err)
	require.NoError(t, cli.FetchCSRFTokenForTesting(ctx))

	dir := testutil.TempDirectory(t)
	si := env.LocalPathSourceInfo(dir)

	data := make([]byte, size)

	_, err = rand.Read(data)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "f"), data, 0o600))

	mustCreateSource(t, cli, dir, &policy.Policy{})

	snapshotNow := func(count int) *serverapi.Snapshot {
		t.Helper()

		_, err := serverapi.UploadSnapshots(ctx, cli, &si)
		require.NoError(t, err)

		var snaps []*serverapi.Snapshot

		require.Eventually(t, func() bool {
			resp, err := serverapi.ListSnapshots(ctx, cli, si, true)
			if err != nil {
				return false
			}

			snaps = resp.Snapshots

			return len(snaps) == count
		}, 30*time.Second, 100*time.Millisecond)

		latest := snaps[0]
		for _, s := range snaps {
			if s.StartTime.After(latest.StartTime) {
				latest = s
			}
		}

		return latest
	}

	first := snapshotNow(1)
	require.NotNil(t, first.UploadedBytes)
	require.GreaterOrEqual(t, *first.UploadedBytes, int64(size))
	require.Less(t, *first.UploadedBytes, int64(size+overhead))

	// the sources list carries the last snapshot's manifest
	var last *snapshot.Manifest

	require.Eventually(t, func() bool {
		srcs := mustListSources(t, cli, &si)
		if len(srcs) != 1 || srcs[0].LastSnapshot == nil {
			return false
		}

		last = srcs[0].LastSnapshot

		return true
	}, 30*time.Second, 100*time.Millisecond)

	require.NotNil(t, last.UploadedBytes)
	require.Equal(t, *first.UploadedBytes, *last.UploadedBytes)

	// nothing changed: nothing new to store
	second := snapshotNow(2)
	require.NotNil(t, second.UploadedBytes)
	require.Less(t, *second.UploadedBytes, int64(overhead))
}

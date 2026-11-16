package server_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/internal/apiclient"
	"github.com/kopia/kopia/internal/repotesting"
	"github.com/kopia/kopia/internal/serverapi"
	"github.com/kopia/kopia/internal/servertesting"
	"github.com/kopia/kopia/internal/testutil"
	"github.com/kopia/kopia/snapshot"
	"github.com/kopia/kopia/snapshot/image"
	"github.com/kopia/kopia/snapshot/policy"
)

// TestImageSourceIsNeverSnapshotted checks that the server lists a local
// image source but never schedules or starts a filesystem snapshot of it,
// even when it inherits a schedule.
func TestImageSourceIsNeverSnapshotted(t *testing.T) {
	ctx, env := repotesting.NewEnvironment(t, repotesting.FormatNotImportant)

	imagePath, err := image.DiskSourcePath("0f6ab3f2-1c3d-4e5f-8a9b-0c1d2e3f4a5b")
	require.NoError(t, err)

	imageSrc := snapshot.SourceInfo{
		Host:     env.Repository.ClientOptions().Hostname,
		UserName: env.Repository.ClientOptions().Username,
		Path:     imagePath,
	}

	every := policy.SchedulingPolicy{IntervalSeconds: 60, RunMissed: policy.NewOptionalBool(true)}

	// a global schedule, which the image source inherits, and a policy that makes the source known
	require.NoError(t, policy.SetPolicy(ctx, env.RepositoryWriter, policy.GlobalPolicySourceInfo, &policy.Policy{SchedulingPolicy: every}))

	keepLatest := policy.OptionalInt(7)

	require.NoError(t, policy.SetPolicy(ctx, env.RepositoryWriter, imageSrc, &policy.Policy{
		RetentionPolicy: policy.RetentionPolicy{KeepLatest: &keepLatest},
	}))
	require.NoError(t, env.RepositoryWriter.Flush(ctx))

	srvInfo := servertesting.StartServer(t, env, false)

	cli, err := apiclient.NewKopiaAPIClient(apiclient.Options{
		BaseURL:                             srvInfo.BaseURL,
		TrustedServerCertificateFingerprint: srvInfo.TrustedServerCertificateFingerprint,
		Username:                            servertesting.TestUIUsername,
		Password:                            servertesting.TestUIPassword,
	})
	require.NoError(t, err)
	require.NoError(t, cli.FetchCSRFTokenForTesting(ctx))

	// a directory source with the same inherited schedule is scheduled as before
	dir := testutil.TempDirectory(t)
	mustCreateSource(t, cli, dir, &policy.Policy{})

	var imageStatus, dirStatus *serverapi.SourceStatus

	require.Eventually(t, func() bool {
		imageStatus, dirStatus = nil, nil

		for _, s := range mustListSources(t, cli, &snapshot.SourceInfo{}) {
			switch s.Source {
			case imageSrc:
				imageStatus = s
			case env.LocalPathSourceInfo(dir):
				dirStatus = s
			}
		}

		return imageStatus != nil && imageStatus.Status == "IMAGE" && dirStatus != nil && dirStatus.NextSnapshotTime != nil
	}, 15*time.Second, 100*time.Millisecond)

	require.Nil(t, imageStatus.NextSnapshotTime, "an image source must not show a next snapshot time")

	// "Snapshot now" does not apply to image sources
	_, err = serverapi.UploadSnapshots(ctx, cli, &imageSrc)
	require.Error(t, err)

	// image sources cannot be added as directories
	_, err = serverapi.CreateSnapshotSource(ctx, cli, &serverapi.CreateSnapshotSourceRequest{
		Path:   imagePath,
		Policy: &policy.Policy{},
	})
	require.ErrorContains(t, err, "kopia image create")

	// nothing ever snapshots the image source
	snaps, err := snapshot.ListSnapshots(ctx, env.Repository, imageSrc)
	require.NoError(t, err)
	require.Empty(t, snaps)
}

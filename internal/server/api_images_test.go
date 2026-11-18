package server_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/pkg/errors"
	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/internal/apiclient"
	"github.com/kopia/kopia/internal/clock"
	"github.com/kopia/kopia/internal/repotesting"
	"github.com/kopia/kopia/internal/serverapi"
	"github.com/kopia/kopia/internal/servertesting"
	"github.com/kopia/kopia/internal/testutil"
	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/snapshot"
	"github.com/kopia/kopia/snapshot/image"
	"github.com/kopia/kopia/snapshot/image/disksource"
	"github.com/kopia/kopia/snapshot/image/recoverymedia"
	"github.com/kopia/kopia/snapshot/image/schedule"
)

// fakeImageHost is an ImageHost with tasks in a map and status files in a
// temporary folder.
type fakeImageHost struct {
	dir string

	mu    sync.Mutex
	tasks map[string]schedule.TaskStatus
	runs  []string

	queryErr error
	runErr   error

	// disks is nil where the host cannot list disks.
	disks []disksource.LocalDisk
}

func (h *fakeImageHost) QueryTask(_ context.Context, name string) (schedule.TaskStatus, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.queryErr != nil {
		return schedule.TaskStatus{}, h.queryErr
	}

	ts, ok := h.tasks[name]
	if !ok {
		return schedule.TaskStatus{}, errors.Wrap(schedule.ErrTaskNotFound, name)
	}

	return ts, nil
}

func (h *fakeImageHost) RunTask(_ context.Context, name string) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.runErr != nil {
		return h.runErr
	}

	h.runs = append(h.runs, name)

	return nil
}

func (h *fakeImageHost) setTask(name string, ts schedule.TaskStatus) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.tasks[name] = ts
}

func (h *fakeImageHost) setQueryError(err error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.queryErr = err
}

func (h *fakeImageHost) setRunError(err error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.runErr = err
}

func (h *fakeImageHost) ranTasks() []string {
	h.mu.Lock()
	defer h.mu.Unlock()

	return slices.Clone(h.runs)
}

func (h *fakeImageHost) StateDir() (string, error) {
	return h.dir, nil
}

func (h *fakeImageHost) Disks() ([]disksource.LocalDisk, error) {
	if h.disks == nil {
		return nil, disksource.ErrNotSupported
	}

	return h.disks, nil
}

func startImageServer(t *testing.T, env *repotesting.Environment, host *fakeImageHost) *apiclient.KopiaAPIClient {
	t.Helper()

	srvInfo := servertesting.StartServerContextWithOptions(t.Context(), t, env, servertesting.StartServerOptions{ImageHost: host})

	cli, err := apiclient.NewKopiaAPIClient(apiclient.Options{
		BaseURL:  srvInfo.BaseURL,
		Username: servertesting.TestUIUsername,
		Password: servertesting.TestUIPassword,
	})
	require.NoError(t, err)
	require.NoError(t, cli.FetchCSRFTokenForTesting(t.Context()))

	return cli
}

func mustCreateRawImage(t *testing.T, env *repotesting.Environment, host, diskGUID string, data []byte) *snapshot.Manifest {
	t.Helper()

	path, err := image.DiskSourcePath(diskGUID)
	require.NoError(t, err)

	man, _, err := image.Create(t.Context(), env.RepositoryWriter, &image.CreateOptions{
		Source: snapshot.SourceInfo{Host: host, UserName: "u", Path: path},
		Layout: &image.Layout{
			DiskGUID:   diskGUID,
			SectorSize: 512,
			DiskSize:   1 << 30,
			Partitions: []image.Partition{{
				Number: 1, TypeGUID: image.PartitionTypeBasicData, UniqueGUID: "11111111-2222-4333-8444-555555555551",
				Offset: 1 << 20, Length: int64(len(data)), Name: "data",
			}},
		},
		Volumes: []image.VolumeSource{{
			PartitionNumber: 1, Label: "D", Kind: image.VolumeKindRaw, Size: int64(len(data)),
			Open: func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil },
		}},
	})
	require.NoError(t, err)

	return man
}

func TestImagesList(t *testing.T) {
	ctx, env := repotesting.NewEnvironment(t, repotesting.FormatNotImportant)

	localHost := env.Repository.ClientOptions().Hostname
	data := bytes.Repeat([]byte{1, 2, 3, 4}, 1<<18)

	local := mustCreateRawImage(t, env, localHost, "0f6ab3f2-1c3d-4e5f-8a9b-0c1d2e3f4a51", data)
	other := mustCreateRawImage(t, env, "other-host", "0f6ab3f2-1c3d-4e5f-8a9b-0c1d2e3f4a52", data)
	require.NoError(t, env.RepositoryWriter.Flush(ctx))

	cli := startImageServer(t, env, &fakeImageHost{dir: testutil.TempDirectory(t)})

	resp, err := serverapi.ListImages(ctx, cli, false)
	require.NoError(t, err)
	require.Equal(t, localHost, resp.LocalHost)
	require.Len(t, resp.Images, 1)

	img := resp.Images[0]
	require.Equal(t, local.ID, img.ID)
	require.Equal(t, local.RootObjectID().String(), img.RootID)
	require.Empty(t, img.MetadataError)
	require.Equal(t, "0f6ab3f2-1c3d-4e5f-8a9b-0c1d2e3f4a51", img.DiskGUID)
	require.Equal(t, int64(len(data)), img.BytesRead)
	require.Len(t, img.Volumes, 1)
	require.Equal(t, image.VolumeKindRaw, img.Volumes[0].Kind)
	require.Equal(t, int64(len(data)), img.Volumes[0].Size)

	// the disk map shows the imaged disk's partitions
	require.Equal(t, int64(1<<30), img.DiskSize)
	require.Equal(t, []serverapi.ImagePartition{{
		Number: 1, Name: "data", TypeGUID: image.PartitionTypeBasicData, Length: int64(len(data)),
	}}, img.Partitions)

	resp, err = serverapi.ListImages(ctx, cli, true)
	require.NoError(t, err)
	require.Len(t, resp.Images, 2)
	require.ElementsMatch(t, []any{local.ID, other.ID}, []any{resp.Images[0].ID, resp.Images[1].ID})
}

func TestImageFiles(t *testing.T) {
	ctx, env := repotesting.NewEnvironment(t, repotesting.FormatNotImportant)

	localHost := env.Repository.ClientOptions().Hostname
	man := mustCreateRawImage(t, env, localHost, "0f6ab3f2-1c3d-4e5f-8a9b-0c1d2e3f4a51", bytes.Repeat([]byte{1}, 1<<20))
	require.NoError(t, env.RepositoryWriter.Flush(ctx))

	cli := startImageServer(t, env, &fakeImageHost{dir: testutil.TempDirectory(t)})
	root := man.RootObjectID().String()

	var resp serverapi.ImageFilesResponse

	require.NoError(t, cli.Get(ctx, "images/files?root="+root, nil, &resp))
	require.Empty(t, resp.Path)

	var names []string

	for _, e := range resp.Entries {
		names = append(names, e.Name)
	}

	// the root holds the volume image and the image's metadata, folders first
	require.Contains(t, names, "p1-D.img")
	require.Contains(t, names, "image.json")
	require.Contains(t, names, "layout.json")

	sawFile := false

	for _, e := range resp.Entries {
		if e.Dir {
			require.False(t, sawFile, "folders are listed before files")
		} else {
			sawFile = true
		}

		if e.Name == "p1-D.img" {
			require.Equal(t, int64(1<<20), e.Size)
		}
	}

	require.Error(t, cli.Get(ctx, "images/files?root="+root+"&path=p1-D.img", nil, &resp), "a file is not a folder")
	require.ErrorIs(t, cli.Get(ctx, "images/files?root="+root+"&path=missing", errNotFoundForTest, &resp), errNotFoundForTest)
	require.Error(t, cli.Get(ctx, "images/files?root=not-an-id", nil, &resp))
}

var errNotFoundForTest = errors.New("not found")

func TestImagesStatus(t *testing.T) {
	ctx, env := repotesting.NewEnvironment(t, repotesting.FormatNotImportant)

	host := &fakeImageHost{dir: testutil.TempDirectory(t), tasks: map[string]schedule.TaskStatus{}}
	cli := startImageServer(t, env, host)

	createTask := schedule.TaskPath(schedule.DefaultTaskFolder, schedule.CreateTaskName)
	checkTask := schedule.TaskPath(schedule.DefaultTaskFolder, schedule.CheckTaskName)

	// nothing installed or run yet
	st, err := serverapi.ImageStatus(ctx, cli)
	require.NoError(t, err)
	require.True(t, st.Supported)
	require.Len(t, st.Tasks, 2)
	require.Equal(t, createTask, st.Tasks[0].Name)
	require.False(t, st.Tasks[0].Scheduled)
	require.Equal(t, checkTask, st.Tasks[1].Name)
	require.False(t, st.Tasks[1].Scheduled)
	require.Nil(t, st.LastRun)
	require.Empty(t, st.LastRunError)
	require.Nil(t, st.RecoveryMedia)
	require.Empty(t, st.RecoveryMediaError)

	// a task, a run and recovery media
	host.setTask(createTask, schedule.TaskStatus{Status: "Ready", NextRunTime: "tomorrow", LastResult: "0"})

	start := time.Date(2026, 11, 16, 2, 0, 0, 0, time.UTC)
	require.NoError(t, disksource.WriteRunStatus(filepath.Join(host.dir, "image-last-run.json"), &disksource.RunStatus{
		Volumes:       []string{"C:"},
		StartTime:     start,
		EndTime:       start.Add(time.Hour),
		Success:       true,
		RecoveryMedia: []string{"the media is old"},
	}))
	require.NoError(t, recoverymedia.WriteRecord(filepath.Join(host.dir, recoverymedia.RecordFileName), &recoverymedia.Record{
		BuiltAt: start, KopiaVersion: "v1", Target: "usb",
	}))

	st, err = serverapi.ImageStatus(ctx, cli)
	require.NoError(t, err)
	require.True(t, st.Tasks[0].Scheduled)
	require.Equal(t, "Ready", st.Tasks[0].Status)
	require.Equal(t, "tomorrow", st.Tasks[0].NextRunTime)
	require.False(t, st.Tasks[1].Scheduled)
	require.NotNil(t, st.LastRun)
	require.True(t, st.LastRun.Success)
	require.Equal(t, []string{"the media is old"}, st.LastRun.RecoveryMedia)
	require.NotNil(t, st.RecoveryMedia)
	require.Equal(t, "usb", st.RecoveryMedia.Target)
	require.True(t, start.Equal(st.RecoveryMedia.BuiltAt))
	require.Empty(t, st.RecoveryMedia.BuiltOn)

	// media built on another computer and registered for this one
	require.NoError(t, recoverymedia.WriteRecord(filepath.Join(host.dir, recoverymedia.RecordFileName), &recoverymedia.Record{
		BuiltAt: start, KopiaVersion: "v1", Target: `E:\`, BuiltOn: "DESKTOP",
	}))

	st, err = serverapi.ImageStatus(ctx, cli)
	require.NoError(t, err)
	require.Equal(t, "DESKTOP", st.RecoveryMedia.BuiltOn)
	require.Equal(t, `E:\`, st.RecoveryMedia.Target)

	// a damaged status file is reported, the rest still shows
	require.NoError(t, os.WriteFile(filepath.Join(host.dir, "image-last-run.json"), []byte("{"), 0o600))

	st, err = serverapi.ImageStatus(ctx, cli)
	require.NoError(t, err)
	require.Nil(t, st.LastRun)
	require.NotEmpty(t, st.LastRunError)
	require.NotNil(t, st.RecoveryMedia)

	// a task that cannot be queried
	host.setQueryError(errors.New("access is denied"))

	st, err = serverapi.ImageStatus(ctx, cli)
	require.NoError(t, err)
	require.Contains(t, st.Tasks[0].Error, "access is denied")
}

func TestImagesStatus_notSupported(t *testing.T) {
	ctx, env := repotesting.NewEnvironment(t, repotesting.FormatNotImportant)

	host := &fakeImageHost{dir: testutil.TempDirectory(t), queryErr: schedule.ErrNotSupported}
	cli := startImageServer(t, env, host)

	st, err := serverapi.ImageStatus(ctx, cli)
	require.NoError(t, err)
	require.False(t, st.Supported)
	require.Empty(t, st.Tasks)

	_, err = serverapi.RunImageTask(ctx, cli)
	require.Error(t, err)
	require.Empty(t, host.ranTasks())
}

func TestImagesScheduleRun(t *testing.T) {
	ctx, env := repotesting.NewEnvironment(t, repotesting.FormatNotImportant)

	host := &fakeImageHost{dir: testutil.TempDirectory(t), tasks: map[string]schedule.TaskStatus{}}
	cli := startImageServer(t, env, host)

	createTask := schedule.TaskPath(schedule.DefaultTaskFolder, schedule.CreateTaskName)

	// not installed
	err := cli.Post(ctx, "images/schedule/run", &serverapi.Empty{}, &serverapi.RunImageTaskResponse{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "kopia image schedule install")
	require.Empty(t, host.ranTasks())

	host.setTask(createTask, schedule.TaskStatus{Status: "Ready"})

	resp, err := serverapi.RunImageTask(ctx, cli)
	require.NoError(t, err)
	require.Equal(t, createTask, resp.Task)
	require.Equal(t, []string{createTask}, host.ranTasks())

	// Task Scheduler refuses an account that is not an administrator:
	// ACCESS_DENIED tells KopiaUI to ask for administrator rights
	host.setRunError(errors.Wrap(schedule.ErrAccessDenied, "schtasks /Run failed"))

	_, err = serverapi.RunImageTask(ctx, cli)

	var statusErr apiclient.HTTPStatusError

	require.ErrorAs(t, err, &statusErr)
	require.Equal(t, http.StatusForbidden, statusErr.HTTPStatusCode)
	require.Contains(t, err.Error(), "administrator")
	require.Equal(t, []string{createTask}, host.ranTasks())
}

// TestImagesAPI_requiresCSRFToken checks that starting the image task is
// protected like the other UI actions.
func TestImagesAPI_requiresCSRFToken(t *testing.T) {
	_, env := repotesting.NewEnvironment(t, repotesting.FormatNotImportant)

	host := &fakeImageHost{dir: testutil.TempDirectory(t), tasks: map[string]schedule.TaskStatus{
		schedule.TaskPath(schedule.DefaultTaskFolder, schedule.CreateTaskName): {Status: "Ready"},
	}}

	srvInfo := servertesting.StartServerContextWithOptions(t.Context(), t, env, servertesting.StartServerOptions{ImageHost: host})

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srvInfo.BaseURL+"/api/v1/images/schedule/run", bytes.NewReader([]byte("{}")))
	require.NoError(t, err)
	req.SetBasicAuth(servertesting.TestUIUsername, servertesting.TestUIPassword)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)

	defer resp.Body.Close()

	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	require.Empty(t, host.ranTasks())
}

// TestImagesPageIsServed checks that the server answers the web UI's
// /images route with the UI page, so that the page can be reloaded.
func TestImagesPageIsServed(t *testing.T) {
	_, env := repotesting.NewEnvironment(t, repotesting.FormatNotImportant)

	srvInfo := servertesting.StartServer(t, env, false)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srvInfo.BaseURL+"/images", http.NoBody)
	require.NoError(t, err)
	req.SetBasicAuth(servertesting.TestUIUsername, servertesting.TestUIPassword)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)

	defer resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Contains(t, resp.Header.Get("Content-Type"), "text/html")
}

// TestImagesStatus_schedule checks that the status reports the schedule
// settings and whether they are for the server's repository.
func TestImagesStatus_schedule(t *testing.T) {
	ctx, env := repotesting.NewEnvironment(t, repotesting.FormatNotImportant)

	host := &fakeImageHost{dir: testutil.TempDirectory(t), tasks: map[string]schedule.TaskStatus{}}
	cli := startImageServer(t, env, host)

	lc, err := repo.LoadConfigFromFile(env.ConfigFile())
	require.NoError(t, err)

	settings := &schedule.Settings{
		Volumes:          []string{"C:"},
		At:               "02:00",
		Days:             []string{"Monday", "Thursday"},
		CheckDay:         "Sunday",
		CheckAt:          "12:00",
		KopiaVersion:     "v0.1",
		ConnectionDigest: recoverymedia.ConnectionDigest(lc),
	}
	require.NoError(t, schedule.WriteSettings(schedule.SettingsPath(host.dir), settings))

	st, err := serverapi.ImageStatus(ctx, cli)
	require.NoError(t, err)
	require.Empty(t, st.ScheduleError)
	require.Equal(t, settings, st.Schedule)
	require.Equal(t, repo.BuildVersion, st.KopiaVersion)
	require.NotNil(t, st.ScheduleForThisRepository)
	require.True(t, *st.ScheduleForThisRepository)

	// tasks that write to another repository
	settings.ConnectionDigest = "another"
	require.NoError(t, schedule.WriteSettings(schedule.SettingsPath(host.dir), settings))

	st, err = serverapi.ImageStatus(ctx, cli)
	require.NoError(t, err)
	require.NotNil(t, st.ScheduleForThisRepository)
	require.False(t, *st.ScheduleForThisRepository)
}

func TestImagesDisks(t *testing.T) {
	ctx, env := repotesting.NewEnvironment(t, repotesting.FormatNotImportant)

	disks := []disksource.LocalDisk{{
		Number: 3, Model: "NVMe", Bus: "NVMe", System: true,
		Volumes: []disksource.LocalVolume{{Letter: "C:", Label: "Windows", FileSystem: "NTFS", Size: 1 << 40}},
	}}

	host := &fakeImageHost{dir: testutil.TempDirectory(t), disks: disks}
	cli := startImageServer(t, env, host)

	resp, err := serverapi.ImageDisks(ctx, cli)
	require.NoError(t, err)
	require.True(t, resp.Supported)
	require.Equal(t, disks, resp.Disks)

	// a host that cannot list disks
	other := startImageServer(t, env, &fakeImageHost{dir: testutil.TempDirectory(t)})

	resp, err = serverapi.ImageDisks(ctx, other)
	require.NoError(t, err)
	require.False(t, resp.Supported)
	require.Empty(t, resp.Disks)
}

// TestImagesStatus_hiddenTasks checks that tasks Task Scheduler hides from
// the server's account, as it hides the tasks of SYSTEM, count as installed
// when the schedule settings say so, and that starting one asks KopiaUI for
// administrator rights instead of saying it is not scheduled.
func TestImagesStatus_hiddenTasks(t *testing.T) {
	ctx, env := repotesting.NewEnvironment(t, repotesting.FormatNotImportant)

	// no task is visible
	host := &fakeImageHost{dir: testutil.TempDirectory(t), tasks: map[string]schedule.TaskStatus{}}
	cli := startImageServer(t, env, host)

	require.NoError(t, schedule.WriteSettings(schedule.SettingsPath(host.dir), &schedule.Settings{
		Volumes: []string{"C:"}, At: "02:00", CheckDay: "none", CheckAt: "12:00",
	}))

	st, err := serverapi.ImageStatus(ctx, cli)
	require.NoError(t, err)
	require.True(t, st.Tasks[0].Scheduled)
	require.True(t, st.Tasks[0].StatusHidden)
	require.Empty(t, st.Tasks[0].Error)

	// the weekly check is off
	require.False(t, st.Tasks[1].Scheduled)
	require.False(t, st.Tasks[1].StatusHidden)

	_, err = serverapi.RunImageTask(ctx, cli)

	var statusErr apiclient.HTTPStatusError

	require.ErrorAs(t, err, &statusErr)
	require.Equal(t, http.StatusForbidden, statusErr.HTTPStatusCode)

	// without settings, nothing is scheduled
	require.NoError(t, os.Remove(schedule.SettingsPath(host.dir)))

	st, err = serverapi.ImageStatus(ctx, cli)
	require.NoError(t, err)
	require.False(t, st.Tasks[0].Scheduled)

	_, err = serverapi.RunImageTask(ctx, cli)
	require.ErrorAs(t, err, &statusErr)
	require.Equal(t, http.StatusNotFound, statusErr.HTTPStatusCode)
}

// TestImagesStatus_progress checks that the status reports the progress of a
// run in progress, ignores a progress file a run left behind, and reports the
// last request.
func TestImagesStatus_progress(t *testing.T) {
	ctx, env := repotesting.NewEnvironment(t, repotesting.FormatNotImportant)

	host := &fakeImageHost{dir: testutil.TempDirectory(t), tasks: map[string]schedule.TaskStatus{}}
	cli := startImageServer(t, env, host)

	st, err := serverapi.ImageStatus(ctx, cli)
	require.NoError(t, err)
	require.Nil(t, st.Progress)
	require.Nil(t, st.LastRequest)

	p := &disksource.RunProgress{
		Volumes: []string{"C:"}, StartTime: clock.Now(), UpdatedAt: clock.Now(), Disk: 1, Disks: 1, DiskNumber: 3,
		ProgressSnapshot: image.ProgressSnapshot{Volume: 3, Volumes: 4, VolumeName: "p3-C.img", BytesDone: 250, BytesTotal: 1000},
		UploadedBytes:    42,
	}
	require.NoError(t, disksource.WriteRunProgress(disksource.ProgressPathIn(host.dir), p))
	require.NoError(t, schedule.WriteRequest(schedule.RequestPath(host.dir), &schedule.Request{ID: "r", Operation: "create", Error: "open repository: invalid password"}))

	st, err = serverapi.ImageStatus(ctx, cli)
	require.NoError(t, err)
	require.NotNil(t, st.Progress)
	require.Equal(t, int64(250), st.Progress.BytesDone)
	require.Equal(t, "p3-C.img", st.Progress.VolumeName)
	require.Equal(t, 4, st.Progress.ProgressSnapshot.Volumes, "the partition count must not clash with the drive letters")
	require.Equal(t, []string{"C:"}, st.Progress.Volumes)

	require.NotNil(t, st.LastRequest)
	require.Equal(t, "create", st.LastRequest.Operation)

	// left by a run that ended without removing it
	p.UpdatedAt = clock.Now().Add(-time.Hour)
	require.NoError(t, disksource.WriteRunProgress(disksource.ProgressPathIn(host.dir), p))

	st, err = serverapi.ImageStatus(ctx, cli)
	require.NoError(t, err)
	require.Nil(t, st.Progress)
}

// TestImageSnapshots_sizeIsBytesRead checks that the snapshot list and the
// sources list report how much of the disk an image read, which the UI shows
// as its size, also for images taken before manifests recorded it.
func TestImageSnapshots_sizeIsBytesRead(t *testing.T) {
	ctx, env := repotesting.NewEnvironment(t, repotesting.FormatNotImportant)

	localHost := env.Repository.ClientOptions().Hostname
	data := bytes.Repeat([]byte{1, 2, 3, 4}, 1<<18)

	current := mustCreateRawImage(t, env, localHost, "0f6ab3f2-1c3d-4e5f-8a9b-0c1d2e3f4a54", data)
	require.NotNil(t, current.ImageBytesRead)
	require.Equal(t, int64(len(data)), *current.ImageBytesRead)

	// an image taken by an earlier version, whose manifest lacks the field:
	// a copy of the manifest for another disk, sharing the image's contents.
	// Rewriting a manifest would mean deleting it, and the deletion can carry
	// the same timestamp as the manifest on coarse clocks, so it may not win.
	olderPath, err := image.DiskSourcePath("0f6ab3f2-1c3d-4e5f-8a9b-0c1d2e3f4a55")
	require.NoError(t, err)

	older := current.Clone()
	older.ID = ""
	older.Source.Path = olderPath
	older.ImageBytesRead = nil
	_, err = snapshot.SaveSnapshot(ctx, env.RepositoryWriter, older)
	require.NoError(t, err)
	require.NoError(t, env.RepositoryWriter.Flush(ctx))

	cli := startImageServer(t, env, &fakeImageHost{dir: testutil.TempDirectory(t)})

	for _, src := range []snapshot.SourceInfo{current.Source, older.Source} {
		snaps, err := serverapi.ListSnapshots(ctx, cli, src, true)
		require.NoError(t, err)
		require.Len(t, snaps.Snapshots, 1)
		require.NotNil(t, snaps.Snapshots[0].ImageBytesRead, src)
		require.Equal(t, int64(len(data)), *snaps.Snapshots[0].ImageBytesRead, src)

		sources, err := serverapi.ListSources(ctx, cli, &src)
		require.NoError(t, err)
		require.Len(t, sources.Sources, 1)
		require.NotNil(t, sources.Sources[0].LastSnapshot.ImageBytesRead, src)
		require.Equal(t, int64(len(data)), *sources.Sources[0].LastSnapshot.ImageBytesRead, src)
	}
}

func TestImagesDrivers(t *testing.T) {
	ctx, env := repotesting.NewEnvironment(t, repotesting.FormatNotImportant)

	cli := startImageServer(t, env, &fakeImageHost{dir: testutil.TempDirectory(t)})

	resp, err := serverapi.ImageDrivers(ctx, cli)
	require.NoError(t, err)
	require.Empty(t, resp.Hosts)

	dir := testutil.TempDirectory(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "rt640x64.inf"), []byte("[Version]"), 0o600))

	path, err := image.DiskSourcePath("0f6ab3f2-1c3d-4e5f-8a9b-0c1d2e3f4a51")
	require.NoError(t, err)

	data := bytes.Repeat([]byte{1, 2, 3, 4}, 1<<18)
	driver := image.Driver{Class: "Net", INF: "rt640x64.inf", Version: "10.65", Entry: "oem40"}

	man, _, err := image.Create(ctx, env.RepositoryWriter, &image.CreateOptions{
		Source: snapshot.SourceInfo{Host: "laptop", UserName: "u", Path: path},
		Layout: &image.Layout{
			DiskGUID:   "0f6ab3f2-1c3d-4e5f-8a9b-0c1d2e3f4a51",
			SectorSize: 512,
			DiskSize:   1 << 30,
			Partitions: []image.Partition{{
				Number: 1, TypeGUID: image.PartitionTypeBasicData, UniqueGUID: "11111111-2222-4333-8444-555555555551",
				Offset: 1 << 20, Length: int64(len(data)), Name: "data",
			}},
		},
		Volumes: []image.VolumeSource{{
			PartitionNumber: 1, Label: "D", Kind: image.VolumeKindRaw, Size: int64(len(data)),
			Open: func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil },
		}},
		Drivers: []image.DriverSource{{Driver: driver, Dir: dir}},
	})
	require.NoError(t, err)

	resp, err = serverapi.ImageDrivers(ctx, cli)
	require.NoError(t, err)
	require.Len(t, resp.Hosts, 1)
	require.Equal(t, "laptop", resp.Hosts[0].Host)
	require.Equal(t, man.ID, resp.Hosts[0].ImageID)
	require.Equal(t, []image.Driver{driver}, resp.Hosts[0].Drivers)

	images, err := serverapi.ListImages(ctx, cli, true)
	require.NoError(t, err)
	require.Len(t, images.Images, 1)
	require.Equal(t, []image.Driver{driver}, images.Images[0].Drivers)
}

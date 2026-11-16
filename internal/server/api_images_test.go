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
	"github.com/kopia/kopia/internal/repotesting"
	"github.com/kopia/kopia/internal/serverapi"
	"github.com/kopia/kopia/internal/servertesting"
	"github.com/kopia/kopia/internal/testutil"
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

func (h *fakeImageHost) RunStatusPath() (string, error) {
	return filepath.Join(h.dir, "image-last-run.json"), nil
}

func (h *fakeImageHost) RecoveryMediaRecordPath() (string, error) {
	return filepath.Join(h.dir, recoverymedia.RecordFileName), nil
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

	resp, err = serverapi.ListImages(ctx, cli, true)
	require.NoError(t, err)
	require.Len(t, resp.Images, 2)
	require.ElementsMatch(t, []any{local.ID, other.ID}, []any{resp.Images[0].ID, resp.Images[1].ID})
}

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

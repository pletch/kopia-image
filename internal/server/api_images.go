package server

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/pkg/errors"

	"github.com/kopia/kopia/internal/serverapi"
	"github.com/kopia/kopia/snapshot"
	"github.com/kopia/kopia/snapshot/image"
	"github.com/kopia/kopia/snapshot/image/disksource"
	"github.com/kopia/kopia/snapshot/image/recoverymedia"
	"github.com/kopia/kopia/snapshot/image/schedule"
)

// ImageHost is kopia image on the computer the server runs on: the
// scheduled image tasks, and the files where image runs and recovery media
// builds record their outcome. The server only reads these and starts the
// scheduled image task, which runs as SYSTEM; it never images disks itself,
// since that needs an administrator.
type ImageHost interface {
	// QueryTask returns a scheduled task's status, an error wrapping
	// schedule.ErrTaskNotFound if it is not installed, or
	// schedule.ErrNotSupported where tasks cannot be scheduled.
	QueryTask(ctx context.Context, name string) (schedule.TaskStatus, error)

	// RunTask starts a scheduled task now.
	RunTask(ctx context.Context, name string) error

	// RunStatusPath is the file where the last image run records its outcome.
	RunStatusPath() (string, error)

	// RecoveryMediaRecordPath is the file that describes the last recovery
	// media built.
	RecoveryMediaRecordPath() (string, error)
}

// windowsImageHost is the ImageHost of Windows Task Scheduler and the image
// state folders under ProgramData. Elsewhere its methods return
// schedule.ErrNotSupported or disksource.ErrNotSupported.
type windowsImageHost struct{}

func (windowsImageHost) QueryTask(ctx context.Context, name string) (schedule.TaskStatus, error) {
	props, err := schedule.Query(ctx, name)
	if err != nil {
		return schedule.TaskStatus{}, err //nolint:wrapcheck
	}

	return schedule.StatusFromProperties(props), nil
}

func (windowsImageHost) RunTask(ctx context.Context, name string) error {
	return schedule.Run(ctx, name) //nolint:wrapcheck
}

func (windowsImageHost) RunStatusPath() (string, error) {
	p, err := disksource.DefaultStatePath()
	if err != nil {
		return "", err //nolint:wrapcheck
	}

	return disksource.StatusPath(p), nil
}

func (windowsImageHost) RecoveryMediaRecordPath() (string, error) {
	dir, err := schedule.DefaultConfigDir()
	if err != nil {
		return "", err //nolint:wrapcheck
	}

	return filepath.Join(dir, recoverymedia.RecordFileName), nil
}

func imageHostOf(rc requestContext) ImageHost {
	if h := rc.srv.getOptions().ImageHost; h != nil {
		return h
	}

	return windowsImageHost{}
}

// handleImagesList lists the image snapshots of the repository's host, or of
// every host with ?all=1.
func handleImagesList(ctx context.Context, rc requestContext) (any, *apiError) {
	mans, err := image.ListSnapshots(ctx, rc.rep)
	if err != nil {
		return nil, internalServerError(err)
	}

	co := rc.rep.ClientOptions()

	if rc.queryParam("all") == "" {
		mans = slices.DeleteFunc(mans, func(m *snapshot.Manifest) bool { return m.Source.Host != co.Hostname })
	}

	slices.SortFunc(mans, func(a, b *snapshot.Manifest) int {
		if s := strings.Compare(a.Source.String(), b.Source.String()); s != 0 {
			return s
		}

		return b.StartTime.ToTime().Compare(a.StartTime.ToTime())
	})

	resp := &serverapi.ImagesResponse{
		Images:        []*serverapi.ImageSnapshot{},
		LocalHost:     co.Hostname,
		LocalUsername: co.Username,
	}

	for _, m := range mans {
		resp.Images = append(resp.Images, convertImageSnapshot(ctx, rc, m))
	}

	return resp, nil
}

func convertImageSnapshot(ctx context.Context, rc requestContext, m *snapshot.Manifest) *serverapi.ImageSnapshot {
	img := &serverapi.ImageSnapshot{
		ID:          m.ID,
		Source:      m.Source,
		Description: m.Description,
		StartTime:   m.StartTime,
		EndTime:     m.EndTime,
		Pins:        append([]string{}, m.Pins...),
		Volumes:     []serverapi.ImageVolume{},
	}

	if m.RootEntry != nil && m.RootEntry.DirSummary != nil {
		img.Size = m.RootEntry.DirSummary.TotalFileSize
	}

	md, err := image.ReadMetadata(ctx, rc.rep, m)
	if err != nil {
		img.MetadataError = err.Error()
		return img
	}

	img.DiskGUID = md.DiskGUID
	img.Totals = md.Totals()
	img.UploadedBytes = md.UploadedBytes

	for i := range md.Volumes {
		v := &md.Volumes[i]

		img.Volumes = append(img.Volumes, serverapi.ImageVolume{
			EntryName:       v.EntryName,
			PartitionNumber: v.PartitionNumber,
			Kind:            v.Kind,
			Size:            v.Size,
			FreeBytes:       v.FreeBytes,
		})
	}

	return img
}

func imageCreateTaskPath() string {
	return schedule.TaskPath(schedule.DefaultTaskFolder, schedule.CreateTaskName)
}

// handleImagesStatus reports the scheduled image tasks, the last image run
// and the last recovery media built on the server's computer. A part that
// cannot be read is reported in the response, not as a failed request, so
// that the rest still shows.
func handleImagesStatus(ctx context.Context, rc requestContext) (any, *apiError) {
	host := imageHostOf(rc)
	resp := &serverapi.ImageStatusResponse{Supported: true, Tasks: []*serverapi.ImageTask{}}

	for _, name := range []string{schedule.CreateTaskName, schedule.CheckTaskName} {
		t := &serverapi.ImageTask{Name: schedule.TaskPath(schedule.DefaultTaskFolder, name)}

		ts, err := host.QueryTask(ctx, t.Name)

		switch {
		// checked first: elsewhere than on Windows ErrTaskNotFound is ErrNotSupported
		case errors.Is(err, schedule.ErrNotSupported):
			return &serverapi.ImageStatusResponse{Tasks: []*serverapi.ImageTask{}}, nil
		case errors.Is(err, schedule.ErrTaskNotFound):
		case err != nil:
			t.Error = err.Error()
		default:
			t.Scheduled = true
			t.TaskStatus = ts
		}

		resp.Tasks = append(resp.Tasks, t)
	}

	resp.LastRun, resp.LastRunError = readImageStatusFile(host.RunStatusPath, disksource.ReadRunStatus)
	resp.RecoveryMedia, resp.RecoveryMediaError = readImageStatusFile(host.RecoveryMediaRecordPath, readRecoveryMedia)

	return resp, nil
}

func readRecoveryMedia(path string) (*serverapi.ImageRecoveryMedia, error) {
	r, err := recoverymedia.ReadRecord(path)
	if err != nil {
		return nil, err //nolint:wrapcheck
	}

	return &serverapi.ImageRecoveryMedia{
		BuiltAt:          r.BuiltAt,
		KopiaVersion:     r.KopiaVersion,
		Target:           r.Target,
		PasswordEmbedded: r.PasswordEmbedded,
	}, nil
}

// readImageStatusFile reads a file that an image run or a recovery media
// build writes. A file that does not exist yet is not an error.
func readImageStatusFile[T any](path func() (string, error), read func(string) (*T, error)) (v *T, errorMessage string) {
	p, err := path()
	if err == nil {
		v, err = read(p)
	}

	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err.Error()
	}

	return v, ""
}

// handleImagesScheduleRun starts the scheduled image task, which creates
// images as SYSTEM, the way "kopia image schedule run" does. When Task
// Scheduler refuses the server's account, it fails with ACCESS_DENIED, so
// that KopiaUI can run "kopia image schedule run" as administrator instead,
// after the UAC prompt.
func handleImagesScheduleRun(ctx context.Context, rc requestContext) (any, *apiError) {
	host := imageHostOf(rc)
	name := imageCreateTaskPath()

	_, err := host.QueryTask(ctx, name)
	if err == nil {
		err = host.RunTask(ctx, name)
	}

	switch {
	case err == nil:
		return &serverapi.RunImageTaskResponse{Task: name}, nil
	case errors.Is(err, schedule.ErrNotSupported):
		return nil, requestError(serverapi.ErrorMalformedRequest, "image tasks can only be scheduled on Windows")
	case errors.Is(err, schedule.ErrTaskNotFound):
		return nil, notFoundError("the image task is not scheduled; set it up with 'kopia image schedule install' as administrator")
	case errors.Is(err, schedule.ErrAccessDenied):
		return nil, &apiError{http.StatusForbidden, serverapi.ErrorAccessDenied, "starting the image task needs administrator rights"}
	default:
		return nil, internalServerError(err)
	}
}

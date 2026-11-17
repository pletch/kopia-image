package server

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/pkg/errors"

	"github.com/kopia/kopia/internal/clock"
	"github.com/kopia/kopia/internal/serverapi"
	"github.com/kopia/kopia/repo"
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

	// StateDir is the folder, readable by every user, where image runs,
	// schedule installs and recovery media builds record what they did.
	StateDir() (string, error)

	// Disks lists the disks that hold a volume with a drive letter, which
	// are the disks the schedule can image.
	Disks() ([]disksource.LocalDisk, error)
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

func (windowsImageHost) StateDir() (string, error) {
	return schedule.StateDir() //nolint:wrapcheck
}

func (windowsImageHost) Disks() ([]disksource.LocalDisk, error) {
	return disksource.LocalDisks() //nolint:wrapcheck
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
		RootID:      m.RootObjectID().String(),
		Volumes:     []serverapi.ImageVolume{},
	}

	if m.RootEntry != nil && m.RootEntry.DirSummary != nil {
		img.Size = m.RootEntry.DirSummary.TotalFileSize
	}

	// the disk map is an extra; the image is listed without it
	if l, err := image.ReadLayout(ctx, rc.rep, m); err == nil {
		img.DiskSize = l.DiskSize

		for _, p := range l.Partitions {
			img.Partitions = append(img.Partitions, serverapi.ImagePartition{
				Number:   p.Number,
				Name:     p.Name,
				TypeGUID: p.TypeGUID,
				Length:   p.Length,
			})
		}
	}

	md, err := image.ReadMetadata(ctx, rc.rep, m)
	if err != nil {
		img.MetadataError = err.Error()
		return img
	}

	img.DiskGUID = md.DiskGUID
	img.Totals = md.Totals()
	img.NewDataBytes = md.NewDataBytes
	img.UploadedBytes = md.UploadedBytes
	img.Drivers = md.Drivers

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
	resp := &serverapi.ImageStatusResponse{Supported: true, Tasks: []*serverapi.ImageTask{}, KopiaVersion: repo.BuildVersion}

	stateDir, stateErr := host.StateDir()
	if stateErr == nil {
		resp.LastRun, resp.LastRunError = readImageStatusFile(disksource.StatusPathIn(stateDir), disksource.ReadRunStatus)
		resp.RecoveryMedia, resp.RecoveryMediaError = readImageStatusFile(filepath.Join(stateDir, recoverymedia.RecordFileName), readRecoveryMedia)
		resp.Schedule, resp.ScheduleError = readImageStatusFile(schedule.SettingsPath(stateDir), schedule.ReadSettings)
		resp.LastRequest, _ = readImageStatusFile(schedule.RequestPath(stateDir), schedule.ReadRequest)

		// a file not updated for a minute belongs to a run that ended without
		// removing it
		if p, _ := readImageStatusFile(disksource.ProgressPathIn(stateDir), disksource.ReadRunProgress); p != nil && clock.Now().Sub(p.UpdatedAt) < imageProgressStale {
			resp.Progress = p
		}
	}

	for _, name := range []string{schedule.CreateTaskName, schedule.CheckTaskName} {
		t := &serverapi.ImageTask{Name: schedule.TaskPath(schedule.DefaultTaskFolder, name)}

		ts, err := host.QueryTask(ctx, t.Name)

		switch {
		case errors.Is(err, schedule.ErrNotSupported):
			return &serverapi.ImageStatusResponse{Tasks: []*serverapi.ImageTask{}}, nil
		case errors.Is(err, schedule.ErrTaskNotFound):
			// Task Scheduler hides the tasks of SYSTEM from other accounts as
			// if they did not exist; the settings tell that they do
			t.Scheduled = scheduleHasTask(resp.Schedule, name)
			t.StatusHidden = t.Scheduled
		case err != nil:
			t.Error = err.Error()
		default:
			t.Scheduled = true
			t.TaskStatus = ts
		}

		resp.Tasks = append(resp.Tasks, t)
	}

	if stateErr != nil {
		resp.LastRunError = stateErr.Error()
		return resp, nil
	}

	if resp.Schedule != nil && resp.Schedule.ConnectionDigest != "" {
		if lc, err := repo.LoadConfigFromFile(rc.srv.getOptions().ConfigFile); err == nil {
			same := recoverymedia.ConnectionDigest(lc) == resp.Schedule.ConnectionDigest
			resp.ScheduleForThisRepository = &same
		}
	}

	return resp, nil
}

// scheduleHasTask reports whether the schedule settings include a task:
// the image task always, the weekly check unless it was turned off.
func scheduleHasTask(s *schedule.Settings, name string) bool {
	if s == nil {
		return false
	}

	return name != schedule.CheckTaskName || (s.CheckDay != "" && s.CheckDay != "none")
}

// scheduleInstalled reports whether "kopia image schedule install" recorded
// settings, which "remove" deletes.
func scheduleInstalled(host ImageHost) bool {
	dir, err := host.StateDir()
	if err != nil {
		return false
	}

	_, err = os.Stat(schedule.SettingsPath(dir)) //nolint:gosec // dir is the image state folder, not request input

	return err == nil
}

// handleImagesDrivers lists the drivers that the images of each computer
// store, which recovery media needs to restore it.
func handleImagesDrivers(ctx context.Context, rc requestContext) (any, *apiError) {
	inv, err := image.DriverInventory(ctx, rc.rep)
	if err != nil {
		return nil, internalServerError(err)
	}

	if inv == nil {
		inv = []image.HostDrivers{}
	}

	return &serverapi.ImageDriversResponse{Hosts: inv}, nil
}

// handleImagesDisks lists the disks of the server's computer that the image
// schedule can image.
func handleImagesDisks(_ context.Context, rc requestContext) (any, *apiError) {
	disks, err := imageHostOf(rc).Disks()

	switch {
	case errors.Is(err, disksource.ErrNotSupported):
		return &serverapi.ImageDisksResponse{Disks: []disksource.LocalDisk{}}, nil
	case err != nil:
		return nil, internalServerError(err)
	}

	return &serverapi.ImageDisksResponse{Supported: true, Disks: disks}, nil
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
		BuiltOn:          r.BuiltOn,
		Drivers:          r.Drivers,
	}, nil
}

// readImageStatusFile reads a file that an image run, a schedule install or
// a recovery media build writes. A file that does not exist yet is not an
// error.
func readImageStatusFile[T any](path string, read func(string) (*T, error)) (v *T, errorMessage string) {
	v, err := read(path)
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

	// a task hidden from the server's account can be started as administrator
	if errors.Is(err, schedule.ErrTaskNotFound) && scheduleInstalled(host) {
		err = schedule.ErrAccessDenied
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

// imageProgressStale is how old a progress file may be before its run counts
// as ended: a run updates it every disksource.ProgressInterval.
const imageProgressStale = 30 * disksource.ProgressInterval

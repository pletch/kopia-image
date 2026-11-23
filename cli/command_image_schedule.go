package cli

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/alecthomas/kingpin/v2"
	"github.com/pkg/errors"

	"github.com/kopia/kopia/internal/clock"
	"github.com/kopia/kopia/internal/passwordpersist"
	"github.com/kopia/kopia/internal/units"
	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/snapshot/image/disksource"
	"github.com/kopia/kopia/snapshot/image/recoverymedia"
	"github.com/kopia/kopia/snapshot/image/schedule"
	"github.com/kopia/kopia/snapshot/image/securedir"
)

const (
	imageScheduleConfigName = "repository.config"

	// imageScheduleExeDir holds the copy of kopia that the tasks run.
	imageScheduleExeDir = "bin"
)

type commandImageSchedule struct {
	install commandImageScheduleInstall
	show    commandImageScheduleShow
	run     commandImageScheduleRun
	remove  commandImageScheduleRemove
	update  commandImageScheduleUpdate
	pause   commandImageSchedulePause
	resume  commandImageSchedulePause
}

func (c *commandImageSchedule) setup(svc advancedAppServices, parent commandParent) {
	cmd := parent.Command("schedule", "Run kopia image create on a schedule with Windows Task Scheduler.")
	c.install.setup(svc, cmd)
	c.show.setup(svc, cmd)
	c.run.setup(svc, cmd)
	c.remove.setup(svc, cmd)
	c.update.setup(svc, cmd)
	c.pause.setup(svc, cmd, false)
	c.resume.setup(svc, cmd, true)
}

// imageScheduleLocation names the scheduled tasks and the folder holding
// their repository config, password, cache and logs.
type imageScheduleLocation struct {
	taskFolder string
	configDir  string
	statePath  string

	// defaultConfigDir is set when configDir is the folder under ProgramData.
	defaultConfigDir bool
}

func (l *imageScheduleLocation) flags(cmd *kingpin.CmdClause) {
	cmd.Flag("task-folder", "Task Scheduler folder of the image tasks").Default(schedule.DefaultTaskFolder).Hidden().StringVar(&l.taskFolder)
	cmd.Flag("config-dir", "Folder for the scheduled tasks' repository config (default: ProgramData\\kopia\\image)").Hidden().StringVar(&l.configDir)
	cmd.Flag("state-file", "Shadow copy state file passed to the scheduled runs").Hidden().StringVar(&l.statePath)
}

func (l *imageScheduleLocation) resolve() error {
	if runtime.GOOS != "windows" {
		return schedule.ErrNotSupported
	}

	if l.configDir == "" {
		dir, err := schedule.DefaultConfigDir()
		if err != nil {
			return err //nolint:wrapcheck
		}

		l.configDir = dir
		l.defaultConfigDir = true
	}

	if l.statePath == "" {
		p, err := disksource.DefaultStatePath()
		if err != nil {
			return err //nolint:wrapcheck
		}

		l.statePath = p
	}

	return nil
}

// stateDir is the folder of the shadow copy state file, ProgramData\kopia by
// default, which also holds the run status, the schedule settings and the
// outcome of the last request: every user can read it.
func (l *imageScheduleLocation) stateDir() string {
	return filepath.Dir(l.statePath)
}

// imageScheduleRequest records the outcome of an install or remove that
// KopiaUI started as administrator without a window, see schedule.Request.
type imageScheduleRequest struct {
	id string
}

func (r *imageScheduleRequest) flag(cmd *kingpin.CmdClause) {
	cmd.Flag("request-id", "Record the outcome under this ID in the image state folder, for KopiaUI").Hidden().StringVar(&r.id)
}

// recording returns act, recording its outcome when a request ID is given.
// act must do all the work that can fail, including opening the repository:
// a failed action ends the process before returning.
func (r *imageScheduleRequest) recording(operation string, loc *imageScheduleLocation, act func(ctx context.Context) error) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		err := act(ctx)

		if r.id != "" {
			if werr := r.record(operation, loc, err); werr != nil {
				log(ctx).Errorf("unable to record the outcome: %v", werr)
			}
		}

		return err
	}
}

func (r *imageScheduleRequest) record(operation string, loc *imageScheduleLocation, opErr error) error {
	if loc.statePath == "" {
		p, err := disksource.DefaultStatePath()
		if err != nil {
			return err //nolint:wrapcheck
		}

		loc.statePath = p
	}

	if def, err := schedule.StateDir(); err == nil && filepath.Clean(def) == filepath.Clean(loc.stateDir()) {
		if err := securedir.Secure(def, securedir.UsersRead); err != nil {
			return errors.Wrap(err, "unable to secure the image state folder")
		}
	}

	req := &schedule.Request{ID: r.id, Operation: operation, Time: clock.Now(), Success: opErr == nil}
	if opErr != nil {
		req.Error = opErr.Error()
	}

	return schedule.WriteRequest(schedule.RequestPath(loc.stateDir()), req) //nolint:wrapcheck
}

// scheduledTaskNames returns the names of the image tasks, without their
// folder: the create tasks of the schedules recorded in the state folder,
// or the single one kopia has always had, and the check task.
func (l *imageScheduleLocation) scheduledTaskNames() []string {
	var s *schedule.Settings

	if r, err := schedule.ReadSettings(schedule.SettingsPath(l.stateDir())); err == nil {
		s = r
	}

	return append(s.CreateTaskNames(), schedule.CheckTaskName)
}

func (l *imageScheduleLocation) taskName(name string) string {
	return schedule.TaskPath(l.taskFolder, name)
}

// taskExe is the copy of kopia that the tasks run. The tasks run as SYSTEM,
// so the program must be one that only administrators can replace.
func (l *imageScheduleLocation) taskExe() string {
	return filepath.Join(l.configDir, imageScheduleExeDir, "kopia.exe")
}

func (l *imageScheduleLocation) configFile() string {
	return filepath.Join(l.configDir, imageScheduleConfigName)
}

type commandImageScheduleInstall struct {
	volumes        []string
	at             string
	days           string
	diskSchedules  []string
	timeLimit      time.Duration
	retries        int
	retryInterval  time.Duration
	wake           bool
	allowOnBattery bool
	flushInterval  time.Duration
	checkDay       string
	checkAt        string
	checkSample    float64
	checkNTFS      bool
	kopiaExe       string

	loc imageScheduleLocation
	req imageScheduleRequest
	svc advancedAppServices
	out textOutput
}

func (c *commandImageScheduleInstall) setup(svc advancedAppServices, parent commandParent) {
	cmd := parent.Command("install", "Create or replace the scheduled image tasks. Run as administrator.")
	cmd.Arg("volume", "A volume on each disk to image at --at on --days, e.g. C:").StringsVar(&c.volumes)
	cmd.Flag("at", "Time of day to create the image (HH:MM)").Default("02:00").StringVar(&c.at)
	cmd.Flag("days", "Days to create the image, e.g. Mon,Thu (default: every day)").StringVar(&c.days)
	cmd.Flag("disk-schedule", "Image the disks holding some volumes on their own schedule, e.g. D:=03:00 or D:,E:=21:30/Mon,Thu; repeat for more").StringsVar(&c.diskSchedules)
	cmd.Flag("time-limit", "Stop a run that takes longer").Default("4h").DurationVar(&c.timeLimit)
	cmd.Flag("retries", "Times to retry a failed run").Default("3").IntVar(&c.retries)
	cmd.Flag("retry-interval", "Time between retries").Default("15m").DurationVar(&c.retryInterval)
	cmd.Flag("wake", "Wake the computer from sleep to run").Default("true").BoolVar(&c.wake)
	cmd.Flag("allow-on-battery", "Start runs on battery power").BoolVar(&c.allowOnBattery)
	cmd.Flag("flush-interval", "How often a run saves upload progress").Default(defaultImageFlushInterval.String()).DurationVar(&c.flushInterval)
	cmd.Flag("check-day", "Day of the weekly image check, or none").Default("Sunday").StringVar(&c.checkDay)
	cmd.Flag("check-at", "Time of day of the weekly image check (HH:MM)").Default("12:00").StringVar(&c.checkAt)
	cmd.Flag("check-sample", "Percentage of chunks the weekly check fetches").Default("1").Float64Var(&c.checkSample)
	cmd.Flag("check-ntfs", "Make the weekly check also check the NTFS file systems in the images (image check --ntfs)").BoolVar(&c.checkNTFS)
	cmd.Flag("kopia-exe", "kopia executable copied for the tasks to run (default: this one)").Hidden().StringVar(&c.kopiaExe)
	c.loc.flags(cmd)
	c.req.flag(cmd)
	cmd.Action(svc.noRepositoryAction(c.req.recording("install", &c.loc, c.runWithRepository)))
	c.svc = svc
	c.out.setup(svc)
}

// runWithRepository opens the repository itself, rather than through a
// repository action, so that a request records a repository that does not
// open.
func (c *commandImageScheduleInstall) runWithRepository(ctx context.Context) (err error) {
	rep, err := c.svc.openRepository(ctx, true)
	if err != nil {
		return errors.Wrap(err, "open repository")
	}

	defer func() {
		if cerr := rep.Close(ctx); cerr != nil {
			err = stderrors.Join(err, errors.Wrap(cerr, "unable to close repository"))
		}
	}()

	return c.run(ctx, rep)
}

func (c *commandImageScheduleInstall) run(ctx context.Context, rep repo.Repository) error {
	if err := c.loc.resolve(); err != nil {
		return err
	}

	schedules, err := c.schedules()
	if err != nil {
		return err
	}

	createTasks, checkTask, err := c.tasks(schedules)
	if err != nil {
		return err
	}

	volumes := allScheduledVolumes(schedules)

	if _, err := disksource.DisksForVolumes(volumes); err != nil {
		return errors.Wrap(err, "unable to find the disks to image")
	}

	// the tasks of schedules that this install drops
	var previous []string
	if s, err := schedule.ReadSettings(schedule.SettingsPath(c.loc.stateDir())); err == nil {
		previous = s.CreateTaskNames()
	}

	if err := c.writeConfig(ctx, rep); err != nil {
		return err
	}

	for _, t := range append(createTasks, checkTask) {
		if t == nil {
			continue
		}

		if err := schedule.Install(ctx, t); err != nil {
			return errors.Wrapf(err, "unable to create the task %v", t.Name)
		}

		next := "not reported"
		if props, err := schedule.Query(ctx, t.Name); err == nil {
			next = props["Next Run Time"]
		}

		c.out.printStdout("Scheduled %q, next run %v.\n", t.Name, next)
	}

	if err := c.removeDroppedTasks(ctx, previous, schedules, checkTask == nil); err != nil {
		return err
	}

	if err := c.writeSettings(rep, schedules); err != nil {
		return err
	}

	c.out.printStdout("The tasks run as SYSTEM, using the copy of kopia and the repository connection in %v.\n", c.loc.configDir)
	c.out.printStdout("Run 'kopia image schedule install' again after upgrading kopia, so the tasks use the new version.\n")
	c.out.printStdout("Create the first image now with 'kopia image schedule run', or interactively with 'kopia image create %v'.\n", strings.Join(volumes, " "))

	return nil
}

// removeDroppedTasks removes the create tasks of previous schedules that
// the new ones do not use, and the check task when it was turned off.
func (c *commandImageScheduleInstall) removeDroppedTasks(ctx context.Context, previous []string, schedules []schedule.DiskSchedule, noCheck bool) error {
	if noCheck {
		previous = append(previous, schedule.CheckTaskName)
	}

	for _, name := range previous {
		if slices.ContainsFunc(schedules, func(d schedule.DiskSchedule) bool { return d.Task == name }) {
			continue
		}

		if err := schedule.Delete(ctx, c.loc.taskName(name)); err != nil && !errors.Is(err, schedule.ErrTaskNotFound) {
			return errors.Wrapf(err, "unable to remove the previous task %q", name)
		}
	}

	return nil
}

// schedules returns the schedules to install: the volumes given as
// arguments at --at on --days, then each --disk-schedule. Schedules at the
// same time on the same days are one, run by one task.
func (c *commandImageScheduleInstall) schedules() ([]schedule.DiskSchedule, error) {
	var (
		out []schedule.DiskSchedule
		err error
	)

	if len(c.volumes) > 0 {
		if out, err = addDiskSchedule(out, c.volumes, c.at, c.days); err != nil {
			return nil, err
		}
	}

	for _, spec := range c.diskSchedules {
		volumes, at, days, perr := parseDiskSchedule(spec)
		if perr != nil {
			return nil, perr
		}

		if out, err = addDiskSchedule(out, volumes, at, days); err != nil {
			return nil, errors.Wrapf(err, "invalid --disk-schedule %q", spec)
		}
	}

	if len(out) == 0 {
		return nil, errors.New("give the volumes to image, as arguments or with --disk-schedule")
	}

	if len(out) > schedule.MaxDiskSchedules {
		return nil, errors.Errorf("at most %v schedules", schedule.MaxDiskSchedules)
	}

	for i := range out {
		out[i].Task = schedule.CreateTaskNameFor(i)
	}

	return out, nil
}

// addDiskSchedule adds a schedule of volumes, at a time of day on days, to
// out, or adds the volumes to the schedule that has the same time and days.
func addDiskSchedule(out []schedule.DiskSchedule, volumes []string, at, days string) ([]schedule.DiskSchedule, error) {
	if _, err := time.Parse("15:04", at); err != nil {
		return nil, errors.Errorf("invalid time of day %q, expected HH:MM", at)
	}

	weekdays, err := parseWeekdays(days)
	if err != nil {
		return nil, err
	}

	// in week order, so that the same days in another order match
	slices.Sort(weekdays)
	weekdays = slices.Compact(weekdays)

	d := schedule.DiskSchedule{At: at}
	for _, w := range weekdays {
		d.Days = append(d.Days, w.String())
	}

	// a volume is a drive letter or the folder a volume is mounted in, kept
	// as given; Windows names are the same in any case
	same := func(v string) func(string) bool {
		return func(o string) bool { return strings.EqualFold(o, v) }
	}

	for _, v := range volumes {
		v = strings.TrimSpace(v)

		switch {
		case v == "":
			return nil, errors.New("empty volume name")
		case slices.ContainsFunc(d.Volumes, same(v)):
			return nil, errors.Errorf("%v is listed twice", v)
		case slices.ContainsFunc(out, func(o schedule.DiskSchedule) bool { return slices.ContainsFunc(o.Volumes, same(v)) }):
			return nil, errors.Errorf("%v is in more than one schedule", v)
		}

		d.Volumes = append(d.Volumes, v)
	}

	for i := range out {
		if out[i].At == d.At && slices.Equal(out[i].Days, d.Days) {
			out[i].Volumes = append(out[i].Volumes, d.Volumes...)
			return out, nil
		}
	}

	return append(out, d), nil
}

// parseDiskSchedule parses a --disk-schedule: VOLUMES=HH:MM[/DAYS], where
// VOLUMES and DAYS are separated by commas. A volume may be the folder a
// volume is mounted in, which may hold "=": the time follows the last one.
func parseDiskSchedule(spec string) (volumes []string, at, days string, err error) {
	i := strings.LastIndex(spec, "=")

	vols, when := "", ""
	if i >= 0 {
		vols, when = spec[:i], spec[i+1:]
	}

	if i < 0 || strings.TrimSpace(vols) == "" {
		return nil, "", "", errors.Errorf("invalid --disk-schedule %q, expected VOLUMES=HH:MM[/DAYS], e.g. D:=03:00/Mon,Thu", spec)
	}

	at, days, _ = strings.Cut(when, "/")

	return strings.Split(vols, ","), strings.TrimSpace(at), days, nil
}

// allScheduledVolumes returns the volumes of all the schedules.
func allScheduledVolumes(schedules []schedule.DiskSchedule) []string {
	var out []string

	for _, d := range schedules {
		out = append(out, d.Volumes...)
	}

	return out
}

// writeSettings records the settings the tasks were scheduled with where the
// web UI can read them.
func (c *commandImageScheduleInstall) writeSettings(rep repo.Repository, schedules []schedule.DiskSchedule) error {
	s := &schedule.Settings{
		Volumes:        allScheduledVolumes(schedules),
		At:             schedules[0].At,
		Days:           schedules[0].Days,
		Disks:          schedules,
		TimeLimit:      c.timeLimit.String(),
		Retries:        c.retries,
		RetryInterval:  c.retryInterval.String(),
		Wake:           c.wake,
		AllowOnBattery: c.allowOnBattery,
		CheckDay:       "none",
		CheckAt:        c.checkAt,
		CheckSample:    c.checkSample,
		CheckNTFS:      c.checkNTFS,
		KopiaVersion:   repo.BuildVersion,
		InstalledAt:    clock.Now(),
	}

	if !strings.EqualFold(c.checkDay, "none") && c.checkDay != "" {
		checkDays, err := parseWeekdays(c.checkDay)
		if err != nil {
			return err
		}

		s.CheckDay = checkDays[0].String()
	}

	if lc, err := repo.LoadConfigFromFile(c.svc.repositoryConfigFileName()); err == nil {
		s.Repository = lc.Description
		s.ConnectionDigest = recoverymedia.ConnectionDigest(lc)
	}

	if s.Repository == "" {
		s.Repository = rep.ClientOptions().Description
	}

	return schedule.WriteSettings(schedule.SettingsPath(c.loc.stateDir()), s) //nolint:wrapcheck
}

// tasks builds the task definitions: one that creates the images of each
// schedule, and the weekly check.
func (c *commandImageScheduleInstall) tasks(schedules []schedule.DiskSchedule) (createTasks []*schedule.Task, checkTask *schedule.Task, err error) {
	exe := c.loc.taskExe()

	common := []string{
		"--config-file", c.loc.configFile(),
		"--no-use-credential-manager",
		"--log-dir", filepath.Join(c.loc.configDir, "logs"),
	}

	for _, d := range schedules {
		at, err := time.Parse("15:04", d.At)
		if err != nil {
			return nil, nil, errors.Errorf("invalid time of day %q, expected HH:MM", d.At)
		}

		days, err := parseWeekdays(strings.Join(d.Days, ","))
		if err != nil {
			return nil, nil, err
		}

		createArgs := append(append([]string(nil), common...), "image", "create")
		createArgs = append(createArgs, d.Volumes...)
		// with several schedules, a run can start while another is going
		createArgs = append(createArgs, "--flush-interval", c.flushInterval.String(), "--state-file", c.loc.statePath, "--wait-for-other-run")

		createTasks = append(createTasks, &schedule.Task{
			Name:             c.loc.taskName(d.Task),
			Description:      "Creates kopia block-level images of the disks holding " + strings.Join(d.Volumes, " "),
			Command:          exe,
			Arguments:        createArgs,
			WorkingDir:       c.loc.configDir,
			At:               at,
			Days:             days,
			TimeLimit:        c.timeLimit,
			RetryInterval:    c.retryInterval,
			RetryCount:       c.retries,
			WakeToRun:        c.wake,
			AllowOnBatteries: c.allowOnBattery,
		})
	}

	if strings.EqualFold(c.checkDay, "none") || c.checkDay == "" {
		return createTasks, nil, nil
	}

	checkDays, err := parseWeekdays(c.checkDay)
	if err != nil {
		return nil, nil, err
	}

	checkAt, err := time.Parse("15:04", c.checkAt)
	if err != nil {
		return nil, nil, errors.Errorf("invalid --check-at %q, expected HH:MM", c.checkAt)
	}

	checkArgs := append(append([]string(nil), common...), "image", "check", "--sample", strconv.FormatFloat(c.checkSample, 'f', -1, 64))
	if c.checkNTFS {
		checkArgs = append(checkArgs, "--ntfs")
	}

	checkTask = &schedule.Task{
		Name:             c.loc.taskName(schedule.CheckTaskName),
		Description:      "Checks the latest kopia images of this computer",
		Command:          exe,
		Arguments:        checkArgs,
		WorkingDir:       c.loc.configDir,
		At:               checkAt,
		Days:             checkDays,
		TimeLimit:        c.timeLimit,
		WakeToRun:        false,
		AllowOnBatteries: c.allowOnBattery,
	}

	return createTasks, checkTask, nil
}

// writeConfig secures the task folder and puts into it a copy of kopia and a
// clone of the current repository connection, with its own cache and with the
// password in a file encrypted for this computer, because SYSTEM cannot read
// the user's Windows Credential Manager. Snapshots keep the current user@host.
func (c *commandImageScheduleInstall) writeConfig(ctx context.Context, rep repo.Repository) error {
	password, err := c.svc.getPasswordFromFlags(ctx, false, true)
	if err != nil {
		return errors.Wrap(err, "unable to get the repository password")
	}

	lc, err := repo.LoadConfigFromFile(c.svc.repositoryConfigFileName())
	if err != nil {
		return errors.Wrap(err, "unable to read the current repository connection")
	}

	if err := c.secureConfigDir(); err != nil {
		return err
	}

	if err := c.installExecutable(); err != nil {
		return err
	}

	clone := *lc
	clone.Caching = lc.Caching.CloneOrDefault()
	clone.Caching.CacheDirectory = filepath.Join(c.loc.configDir, "cache")
	clone.ClientOptions = rep.ClientOptions()

	b, err := json.MarshalIndent(&clone, "", "  ")
	if err != nil {
		return errors.Wrap(err, "unable to encode the repository connection")
	}

	cfg := c.loc.configFile()

	if err := os.WriteFile(cfg, b, 0o600); err != nil { //nolint:mnd
		return errors.Wrap(err, "unable to write the repository connection")
	}

	if err := passwordpersist.MachineFile().PersistPassword(ctx, cfg, password); err != nil {
		return errors.Wrap(err, "unable to save the repository password")
	}

	// prove that the copied connection works before scheduling it
	r, err := repo.Open(ctx, cfg, password, &repo.Options{})
	if err != nil {
		return errors.Wrap(err, "the copied repository connection does not work")
	}

	return errors.Wrap(r.Close(ctx), "unable to close the repository")
}

// secureConfigDir makes the task folder one that only SYSTEM and
// Administrators can open. The default folder's parent under ProgramData is
// secured first, because whoever controls a parent can replace the folder.
func (c *commandImageScheduleInstall) secureConfigDir() error {
	if c.loc.defaultConfigDir {
		if err := securedir.Secure(filepath.Dir(c.loc.configDir), securedir.UsersRead); err != nil {
			return errors.Wrap(err, "unable to secure the image state folder")
		}
	}

	return errors.Wrap(securedir.Secure(c.loc.configDir, securedir.AdminsOnly), "unable to secure the task folder")
}

// installExecutable copies kopia into the secured task folder. The tasks run
// the copy as SYSTEM: the original may be in a folder its user can write,
// such as a per-user installation, and replacing it would then run any
// program as SYSTEM.
func (c *commandImageScheduleInstall) installExecutable() error {
	return installTaskExecutable(&c.loc, c.kopiaExe)
}

// installTaskExecutable copies src, or the running kopia when src is "",
// into the task folder of loc.
func installTaskExecutable(loc *imageScheduleLocation, src string) error {
	if src == "" {
		exe, err := os.Executable()
		if err != nil {
			return errors.Wrap(err, "unable to find the kopia executable")
		}

		src = exe
	}

	dst := loc.taskExe()

	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil { //nolint:mnd
		return errors.Wrapf(err, "unable to create %v", filepath.Dir(dst))
	}

	// installing again with the tasks' own copy
	if srcInfo, err := os.Stat(src); err == nil {
		if dstInfo, err := os.Stat(dst); err == nil && os.SameFile(srcInfo, dstInfo) {
			return nil
		}
	}

	if err := copyFile(src, dst); err != nil {
		return errors.Wrapf(err, "unable to copy %v to %v; if a scheduled image run is in progress, wait for it to finish", src, dst)
	}

	return nil
}

// copyFile replaces dst with a copy of src, without leaving a partial dst.
func copyFile(src, dst string) (err error) {
	in, err := os.Open(src) //nolint:gosec
	if err != nil {
		return err //nolint:wrapcheck
	}
	defer in.Close() //nolint:errcheck

	tmp := dst + ".new"

	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o700) //nolint:gosec,mnd
	if err != nil {
		return err //nolint:wrapcheck
	}

	defer func() {
		if err != nil {
			err = stderrors.Join(err, os.Remove(tmp))
		}
	}()

	_, copyErr := io.Copy(out, in)
	if joined := stderrors.Join(copyErr, out.Close()); joined != nil {
		return joined
	}

	return replaceFile(tmp, dst)
}

// replaceFile renames src to dst, replacing it. Windows does not replace a
// program that is running, as the tasks' kopia is during an image run, but
// lets it be renamed: the running program goes on from the renamed file,
// which is removed by the next replacement once nothing runs it.
func replaceFile(src, dst string) error {
	old := dst + ".old"

	// left by an earlier replacement; still in use if that run goes on
	_ = os.Remove(old)

	err := os.Rename(src, dst)
	if err == nil {
		return nil
	}

	if rerr := os.Rename(dst, old); rerr != nil {
		return err //nolint:wrapcheck
	}

	if err := os.Rename(src, dst); err != nil {
		return stderrors.Join(err, os.Rename(old, dst))
	}

	return nil
}

func parseWeekdays(s string) ([]time.Weekday, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}

	var out []time.Weekday

	for part := range strings.SplitSeq(s, ",") {
		p := strings.ToLower(strings.TrimSpace(part))
		found := false

		for d := time.Sunday; d <= time.Saturday; d++ {
			name := strings.ToLower(d.String())
			if p == name || (len(p) >= 3 && strings.HasPrefix(name, p)) {
				out = append(out, d)
				found = true

				break
			}
		}

		if !found {
			return nil, errors.Errorf("invalid day %q", part)
		}
	}

	return out, nil
}

type commandImageScheduleShow struct {
	loc imageScheduleLocation
	out textOutput
}

func (c *commandImageScheduleShow) setup(svc appServices, parent commandParent) {
	cmd := parent.Command("show", "Show the scheduled image tasks and the last run.")
	c.loc.flags(cmd)
	cmd.Action(svc.noRepositoryAction(c.run))
	c.out.setup(svc)
}

func (c *commandImageScheduleShow) run(ctx context.Context) error {
	if err := c.loc.resolve(); err != nil {
		return err
	}

	for _, name := range c.loc.scheduledTaskNames() {
		full := c.loc.taskName(name)

		props, err := schedule.Query(ctx, full)
		if errors.Is(err, schedule.ErrTaskNotFound) {
			c.out.printStdout("%v: not scheduled\n", full)
			continue
		}

		if err != nil {
			return err //nolint:wrapcheck
		}

		ts := schedule.StatusFromProperties(props)

		c.out.printStdout("%v: %v, next run %v, last run %v (result %v)\n",
			full, ts.Status, ts.NextRunTime, ts.LastRunTime, ts.LastResult)
	}

	st, err := disksource.ReadRunStatus(disksource.StatusPath(c.loc.statePath))
	if errors.Is(err, os.ErrNotExist) {
		c.out.printStdout("No image run has completed yet.\n")
		return nil
	}

	if err != nil {
		return err //nolint:wrapcheck
	}

	result := "succeeded"
	if !st.Success {
		result = "FAILED: " + st.Error
	}

	c.out.printStdout("Last image run of %v at %v took %v and %v\n",
		strings.Join(st.Volumes, " "), formatTimestamp(st.StartTime), st.EndTime.Sub(st.StartTime).Truncate(time.Second), result)

	for _, img := range st.Images {
		c.out.printStdout("  %v %v: read %v, new data %v, stored %v\n", img.Source, img.SnapshotID,
			units.BytesString(img.BytesRead), optionalBytes(img.NewDataBytes, "?"), optionalBytes(img.UploadedBytes, "?"))
	}

	if len(st.RecoveryMedia) > 0 {
		c.out.printStdout("Build new recovery media with 'kopia image recovery-media create':\n")

		for _, f := range st.RecoveryMedia {
			c.out.printStdout("  - %v\n", f)
		}
	}

	return nil
}

type commandImageScheduleRun struct {
	volume string

	loc imageScheduleLocation
	out textOutput
}

func (c *commandImageScheduleRun) setup(svc appServices, parent commandParent) {
	cmd := parent.Command("run", "Start the scheduled image tasks now: each images its disks after the one before ends.")
	cmd.Arg("volume", "Start only the task of the schedule that images this volume, e.g. D:").StringVar(&c.volume)
	c.loc.flags(cmd)
	cmd.Action(svc.noRepositoryAction(c.run))
	c.out.setup(svc)
}

func (c *commandImageScheduleRun) run(ctx context.Context) error {
	if err := c.loc.resolve(); err != nil {
		return err
	}

	names, err := c.taskNames()
	if err != nil {
		return err
	}

	for _, name := range names {
		if err := schedule.Run(ctx, c.loc.taskName(name)); err != nil {
			return err //nolint:wrapcheck
		}

		c.out.printStdout("Started %q.\n", c.loc.taskName(name))
	}

	c.out.printStdout("Follow the run with 'kopia image schedule show'.\n")

	return nil
}

// taskNames returns the create tasks to start: all of them, or the one
// whose schedule images the volume given.
func (c *commandImageScheduleRun) taskNames() ([]string, error) {
	s, err := schedule.ReadSettings(schedule.SettingsPath(c.loc.stateDir()))
	if err != nil {
		s = nil
	}

	if c.volume == "" {
		return s.CreateTaskNames(), nil
	}

	if s != nil {
		for _, d := range s.Schedules() {
			if slices.ContainsFunc(d.Volumes, func(v string) bool { return strings.EqualFold(v, c.volume) }) {
				return []string{d.Task}, nil
			}
		}
	}

	return nil, errors.Errorf("no image schedule images %v", c.volume)
}

type commandImageScheduleRemove struct {
	keepConfig bool

	loc imageScheduleLocation
	req imageScheduleRequest
	out textOutput
}

func (c *commandImageScheduleRemove) setup(svc appServices, parent commandParent) {
	cmd := parent.Command("remove", "Remove the scheduled image tasks and their repository connection.")
	cmd.Flag("keep-config", "Keep the tasks' repository connection, password and cache").BoolVar(&c.keepConfig)
	c.loc.flags(cmd)
	c.req.flag(cmd)
	cmd.Action(svc.noRepositoryAction(c.req.recording("remove", &c.loc, c.run)))
	c.out.setup(svc)
}

func (c *commandImageScheduleRemove) run(ctx context.Context) error {
	if err := c.loc.resolve(); err != nil {
		return err
	}

	for _, name := range c.loc.scheduledTaskNames() {
		err := schedule.Delete(ctx, c.loc.taskName(name))

		switch {
		case errors.Is(err, schedule.ErrTaskNotFound):
		case err != nil:
			return err //nolint:wrapcheck
		default:
			c.out.printStdout("Removed %q.\n", c.loc.taskName(name))
		}
	}

	if err := os.Remove(schedule.SettingsPath(c.loc.stateDir())); err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.Wrap(err, "unable to remove the image schedule settings")
	}

	if c.keepConfig {
		return nil
	}

	if err := os.RemoveAll(c.loc.configDir); err != nil {
		// an image run that is still going holds its kopia and log files
		left, lerr := removeAtRestart(c.loc.configDir)
		if lerr != nil {
			return errors.Wrapf(stderrors.Join(err, lerr), "unable to remove %v", c.loc.configDir)
		}

		if _, serr := os.Lstat(c.loc.configFile() + ".kopia-password"); !errors.Is(serr, os.ErrNotExist) {
			return errors.Wrapf(err, "unable to remove the saved repository password in %v", c.loc.configDir)
		}

		c.out.printStdout("Removed the saved repository password. These files are in use, likely by an image run that is still going, and are removed when Windows restarts:\n")

		for _, f := range left {
			c.out.printStdout("  %v\n", f)
		}

		return nil
	}

	c.out.printStdout("Removed %v, including the saved repository password.\n", c.loc.configDir)

	return nil
}

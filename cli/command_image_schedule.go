package cli

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/alecthomas/kingpin/v2"
	"github.com/pkg/errors"

	"github.com/kopia/kopia/internal/passwordpersist"
	"github.com/kopia/kopia/internal/units"
	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/snapshot/image/disksource"
	"github.com/kopia/kopia/snapshot/image/schedule"
	"github.com/kopia/kopia/snapshot/image/securedir"
)

const (
	defaultImageTaskFolder  = `\Kopia`
	imageCreateTaskName     = "Image create"
	imageCheckTaskName      = "Image check"
	imageScheduleConfigName = "repository.config"

	// imageScheduleExeDir holds the copy of kopia that the tasks run.
	imageScheduleExeDir = "bin"
)

type commandImageSchedule struct {
	install commandImageScheduleInstall
	show    commandImageScheduleShow
	run     commandImageScheduleRun
	remove  commandImageScheduleRemove
}

func (c *commandImageSchedule) setup(svc advancedAppServices, parent commandParent) {
	cmd := parent.Command("schedule", "Run kopia image create on a schedule with Windows Task Scheduler.")
	c.install.setup(svc, cmd)
	c.show.setup(svc, cmd)
	c.run.setup(svc, cmd)
	c.remove.setup(svc, cmd)
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
	cmd.Flag("task-folder", "Task Scheduler folder of the image tasks").Default(defaultImageTaskFolder).Hidden().StringVar(&l.taskFolder)
	cmd.Flag("config-dir", "Folder for the scheduled tasks' repository config (default: ProgramData\\kopia\\image)").Hidden().StringVar(&l.configDir)
	cmd.Flag("state-file", "Shadow copy state file passed to the scheduled runs").Hidden().StringVar(&l.statePath)
}

func (l *imageScheduleLocation) resolve() error {
	if runtime.GOOS != "windows" {
		return schedule.ErrNotSupported
	}

	if l.configDir == "" {
		pd, err := schedule.ProgramDataDir()
		if err != nil {
			return err //nolint:wrapcheck
		}

		l.configDir = filepath.Join(pd, "kopia", "image")
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

func (l *imageScheduleLocation) taskName(name string) string {
	return strings.TrimSuffix(l.taskFolder, `\`) + `\` + name
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
	svc advancedAppServices
	out textOutput
}

func (c *commandImageScheduleInstall) setup(svc advancedAppServices, parent commandParent) {
	cmd := parent.Command("install", "Create or replace the scheduled image tasks. Run as administrator.")
	cmd.Arg("volume", "A volume on each disk to image, e.g. C:").Required().StringsVar(&c.volumes)
	cmd.Flag("at", "Time of day to create the image (HH:MM)").Default("02:00").StringVar(&c.at)
	cmd.Flag("days", "Days to create the image, e.g. Mon,Thu (default: every day)").StringVar(&c.days)
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
	cmd.Action(svc.repositoryReaderAction(c.run))
	c.svc = svc
	c.out.setup(svc)
}

func (c *commandImageScheduleInstall) run(ctx context.Context, rep repo.Repository) error {
	if err := c.loc.resolve(); err != nil {
		return err
	}

	createTask, checkTask, err := c.tasks()
	if err != nil {
		return err
	}

	if _, err := disksource.DisksForVolumes(c.volumes); err != nil {
		return errors.Wrap(err, "unable to find the disks to image")
	}

	if err := c.writeConfig(ctx, rep); err != nil {
		return err
	}

	for _, t := range []*schedule.Task{createTask, checkTask} {
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

	if checkTask == nil {
		if err := schedule.Delete(ctx, c.loc.taskName(imageCheckTaskName)); err != nil && !errors.Is(err, schedule.ErrTaskNotFound) {
			return errors.Wrap(err, "unable to remove the previous check task")
		}
	}

	c.out.printStdout("The tasks run as SYSTEM, using the copy of kopia and the repository connection in %v.\n", c.loc.configDir)
	c.out.printStdout("Run 'kopia image schedule install' again after upgrading kopia, so the tasks use the new version.\n")
	c.out.printStdout("Create the first image now with 'kopia image schedule run', or interactively with 'kopia image create %v'.\n", strings.Join(c.volumes, " "))

	return nil
}

// tasks builds the image and weekly check task definitions.
func (c *commandImageScheduleInstall) tasks() (createTask, checkTask *schedule.Task, err error) {
	at, err := time.Parse("15:04", c.at)
	if err != nil {
		return nil, nil, errors.Errorf("invalid --at %q, expected HH:MM", c.at)
	}

	days, err := parseWeekdays(c.days)
	if err != nil {
		return nil, nil, err
	}

	exe := c.loc.taskExe()

	common := []string{
		"--config-file", c.loc.configFile(),
		"--no-use-credential-manager",
		"--log-dir", filepath.Join(c.loc.configDir, "logs"),
	}

	createArgs := append(append([]string(nil), common...), "image", "create")
	createArgs = append(createArgs, c.volumes...)
	createArgs = append(createArgs, "--flush-interval", c.flushInterval.String(), "--state-file", c.loc.statePath)

	createTask = &schedule.Task{
		Name:             c.loc.taskName(imageCreateTaskName),
		Description:      "Creates kopia block-level images of the disks holding " + strings.Join(c.volumes, " "),
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
	}

	if strings.EqualFold(c.checkDay, "none") || c.checkDay == "" {
		return createTask, nil, nil
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
		Name:             c.loc.taskName(imageCheckTaskName),
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

	return createTask, checkTask, nil
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
	src := c.kopiaExe
	if src == "" {
		exe, err := os.Executable()
		if err != nil {
			return errors.Wrap(err, "unable to find the kopia executable")
		}

		src = exe
	}

	dst := c.loc.taskExe()

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

	return os.Rename(tmp, dst) //nolint:wrapcheck
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

	for _, name := range []string{imageCreateTaskName, imageCheckTaskName} {
		full := c.loc.taskName(name)

		props, err := schedule.Query(ctx, full)
		if errors.Is(err, schedule.ErrTaskNotFound) {
			c.out.printStdout("%v: not scheduled\n", full)
			continue
		}

		if err != nil {
			return err //nolint:wrapcheck
		}

		c.out.printStdout("%v: %v, next run %v, last run %v (result %v)\n",
			full, props["Status"], props["Next Run Time"], props["Last Run Time"], props["Last Result"])
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
		uploaded := "?"
		if img.UploadedBytes != nil {
			uploaded = units.BytesString(*img.UploadedBytes)
		}

		c.out.printStdout("  %v %v: read %v, uploaded %v\n", img.Source, img.SnapshotID, units.BytesString(img.BytesRead), uploaded)
	}

	return nil
}

type commandImageScheduleRun struct {
	loc imageScheduleLocation
	out textOutput
}

func (c *commandImageScheduleRun) setup(svc appServices, parent commandParent) {
	cmd := parent.Command("run", "Start the scheduled image task now.")
	c.loc.flags(cmd)
	cmd.Action(svc.noRepositoryAction(c.run))
	c.out.setup(svc)
}

func (c *commandImageScheduleRun) run(ctx context.Context) error {
	if err := c.loc.resolve(); err != nil {
		return err
	}

	if err := schedule.Run(ctx, c.loc.taskName(imageCreateTaskName)); err != nil {
		return err //nolint:wrapcheck
	}

	c.out.printStdout("Started %q; follow it with 'kopia image schedule show'.\n", c.loc.taskName(imageCreateTaskName))

	return nil
}

type commandImageScheduleRemove struct {
	keepConfig bool

	loc imageScheduleLocation
	out textOutput
}

func (c *commandImageScheduleRemove) setup(svc appServices, parent commandParent) {
	cmd := parent.Command("remove", "Remove the scheduled image tasks and their repository connection.")
	cmd.Flag("keep-config", "Keep the tasks' repository connection, password and cache").BoolVar(&c.keepConfig)
	c.loc.flags(cmd)
	cmd.Action(svc.noRepositoryAction(c.run))
	c.out.setup(svc)
}

func (c *commandImageScheduleRemove) run(ctx context.Context) error {
	if err := c.loc.resolve(); err != nil {
		return err
	}

	for _, name := range []string{imageCreateTaskName, imageCheckTaskName} {
		err := schedule.Delete(ctx, c.loc.taskName(name))

		switch {
		case errors.Is(err, schedule.ErrTaskNotFound):
		case err != nil:
			return err //nolint:wrapcheck
		default:
			c.out.printStdout("Removed %q.\n", c.loc.taskName(name))
		}
	}

	if c.keepConfig {
		return nil
	}

	if err := os.RemoveAll(c.loc.configDir); err != nil {
		return errors.Wrapf(err, "unable to remove %v", c.loc.configDir)
	}

	c.out.printStdout("Removed %v, including the saved repository password.\n", c.loc.configDir)

	return nil
}

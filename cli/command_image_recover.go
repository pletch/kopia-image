package cli

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/pkg/errors"

	"github.com/kopia/kopia/internal/clock"
	"github.com/kopia/kopia/internal/units"
	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/snapshot"
	"github.com/kopia/kopia/snapshot/image"
	"github.com/kopia/kopia/snapshot/image/imagerestore"
	"github.com/kopia/kopia/snapshot/image/recoverymedia"
)

const (
	defaultRecoverNetworkWait = 60 * time.Second
	recoverProbeTimeout       = 10 * time.Second
	recoverProbeRetry         = 3 * time.Second
	recoverMaxClockSkew       = 5 * time.Minute
	recoverImagesShown        = 15

	recoverResultFileName = "kopia-result.txt"

	recoverAfterExit     = "exit"
	recoverAfterReboot   = "reboot"
	recoverAfterShutdown = "shutdown"
)

// recoverAnswers answers the questions of "image recover" for unattended
// runs, such as tests of recovery media.
type recoverAnswers struct {
	// Password is the repository password.
	Password string `json:"password,omitempty"`

	// Image is a snapshot ID or an image source; empty means the newest
	// image of the computer the media was built for.
	Image string `json:"image,omitempty"`

	// Target is the disk, or a disk image file, to restore to.
	Target string `json:"target"`

	// Confirm must be true to erase the target.
	Confirm bool `json:"confirm"`

	// Force allows a target while another disk has the image's disk GUID.
	Force bool `json:"force,omitempty"`

	// SetClock sets the computer's clock to the repository server's when
	// they differ.
	SetClock bool `json:"setClock,omitempty"`

	// After is what to do once restored: exit, reboot or shutdown.
	After string `json:"after,omitempty"`
}

type commandImageRecover struct {
	recoveryFile string
	answersFile  string
	target       string
	networkWait  time.Duration
	parallel     int

	answers *recoverAnswers
	info    *recoverymedia.Info
	in      *bufio.Reader

	// media is the root of the recovery media in the recovery environment.
	media string

	svc advancedAppServices
	out textOutput
}

func (c *commandImageRecover) setup(svc advancedAppServices, parent commandParent) {
	cmd := parent.Command("recover", "Restore a disk image step by step, as kopia recovery media does.")
	cmd.Flag("recovery-file", "Recovery description written by 'kopia image recovery-media create'").StringVar(&c.recoveryFile)
	cmd.Flag("answers", "JSON file answering every question, for unattended recovery").StringVar(&c.answersFile)
	cmd.Flag("target", "Disk or disk image file to restore to, instead of choosing one").StringVar(&c.target)
	cmd.Flag("network-wait", "How long to wait for the repository to answer").Default(defaultRecoverNetworkWait.String()).DurationVar(&c.networkWait)
	cmd.Flag("parallel", "Number of chunks fetched in parallel").Default("16").IntVar(&c.parallel)
	cmd.Action(svc.noRepositoryAction(c.run))

	c.svc = svc
	c.out.setup(svc)
}

func (c *commandImageRecover) run(ctx context.Context) error {
	err := c.loadInputs()
	if err == nil {
		err = c.recover(ctx)
	}

	c.writeResult(err)

	if err != nil {
		return err
	}

	return c.finish(ctx)
}

// writeResult records the outcome on the recovery media, where an
// unattended test reads it after the recovery system shuts down.
func (c *commandImageRecover) writeResult(err error) {
	if c.media == "" {
		return
	}

	result := "succeeded " + clock.Now().UTC().Format(time.RFC3339) + "\r\n"
	if err != nil {
		result = "failed " + clock.Now().UTC().Format(time.RFC3339) + ": " + err.Error() + "\r\n"
	}

	if werr := os.WriteFile(filepath.Join(c.media, recoverResultFileName), []byte(result), 0o600); werr != nil { //nolint:mnd
		c.out.printStderr("Unable to write the result to the recovery media: %v\n", werr)
	}
}

func (c *commandImageRecover) recover(ctx context.Context) error {
	cfgPath := c.svc.repositoryConfigFileName()

	lc, err := repo.LoadConfigFromFile(cfgPath)
	if err != nil {
		return errors.Wrap(err, "no repository connection: connect with 'kopia repository connect' first, as read-only as you can")
	}

	c.out.printStdout("Repository: %v\n", recoverymedia.DescribeRepository(lc))

	if err := recoverymedia.TrustCertificates(c.info.CACerts); err != nil {
		c.out.printStderr("Unable to add the recovery media's certificate authorities: %v\n", err)
	}

	if err := c.relocateLocalRepository(cfgPath, lc); err != nil {
		return err
	}

	// reread: relocating changes the repository folder, which must not be restored over
	if lc, err = repo.LoadConfigFromFile(cfgPath); err != nil {
		return errors.Wrap(err, "unable to read the repository connection")
	}

	c.checkNetworkAndClock(ctx, lc)

	rep, err := c.openRepository(ctx, cfgPath)
	if err != nil {
		return err
	}

	defer rep.Close(ctx) //nolint:errcheck

	man, err := c.chooseImage(ctx, rep)
	if err != nil {
		return err
	}

	s, err := imagerestore.LoadSnapshot(ctx, rep, man)
	if err != nil {
		return errors.Wrapf(err, "unable to load image snapshot %v", man.ID)
	}

	printImageRestorePlan(&c.out, s, s.Metadata.Volumes)

	target, err := c.chooseTarget(s, recoverymedia.LocalRepositoryPath(lc))
	if err != nil {
		return err
	}

	return c.restore(ctx, rep, s, target)
}

func (c *commandImageRecover) loadInputs() error {
	c.in = bufio.NewReader(c.svc.stdin())
	c.info = &recoverymedia.Info{}

	if c.recoveryFile != "" {
		info, err := recoverymedia.ReadInfo(c.recoveryFile)
		if err != nil {
			return err //nolint:wrapcheck
		}

		c.info = info
		c.out.printStdout("Kopia recovery media built on %v for %v.\n", formatTimestamp(info.BuiltAt), info.Computer)
	}

	if imagerestore.InRecoveryEnvironment() {
		media, err := recoverymedia.FindMedia()
		if err != nil {
			c.out.printStderr("Unable to find the recovery media: %v\n", err)
		}

		c.media = media

		// only "recovery-media create --answers", meant for tests, puts answers on the media
		if p := filepath.Join(media, recoverymedia.AnswersFileName); media != "" && c.answersFile == "" {
			if _, err := os.Stat(p); err == nil {
				c.out.printStdout("Answering from %v.\n", p)
				c.answersFile = p
			}
		}
	}

	if c.answersFile == "" {
		return nil
	}

	b, err := os.ReadFile(c.answersFile)
	if err != nil {
		return errors.Wrap(err, "unable to read the answers")
	}

	c.answers = &recoverAnswers{}

	return errors.Wrapf(json.Unmarshal(b, c.answers), "invalid answers in %v", c.answersFile)
}

// ask asks a question and returns the trimmed answer.
func (c *commandImageRecover) ask(prompt string) (string, error) {
	c.out.printStdout("%v", prompt)

	line, err := c.in.ReadString('\n')
	if err != nil && line == "" {
		return "", errors.Wrap(err, "no answer")
	}

	return strings.TrimSpace(line), nil
}

func (c *commandImageRecover) askYesNo(prompt string, def bool) (bool, error) {
	a, err := c.ask(prompt)
	if err != nil {
		return false, err
	}

	switch strings.ToLower(a) {
	case "":
		return def, nil
	case "y", "yes":
		return true, nil
	default:
		return false, nil
	}
}

// relocateLocalRepository finds a repository on a local disk whose drive
// letter changed, as drive letters in the recovery system differ from
// Windows', and updates the connection.
func (c *commandImageRecover) relocateLocalRepository(cfgPath string, lc *repo.LocalConfig) error {
	p := recoverymedia.LocalRepositoryPath(lc)
	if p == "" || !isWindows() {
		return nil
	}

	if _, err := os.Stat(p); err == nil {
		return nil
	}

	vol := filepath.VolumeName(p)
	if len(vol) != 2 || vol[1] != ':' {
		return nil
	}

	for d := 'C'; d <= 'Z'; d++ {
		candidate := string(d) + ":" + p[len(vol):]

		if _, err := os.Stat(filepath.Join(candidate, "kopia.repository.f")); err != nil {
			continue
		}

		c.out.printStdout("The repository folder %v is now %v.\n", p, candidate)

		return errors.Wrap(setLocalRepositoryPath(cfgPath, candidate), "unable to update the repository connection")
	}

	return nil
}

// setLocalRepositoryPath changes the folder of a filesystem repository in a
// connection file, keeping everything else as written.
func setLocalRepositoryPath(cfgPath, path string) error {
	b, err := os.ReadFile(cfgPath) //nolint:gosec
	if err != nil {
		return err //nolint:wrapcheck
	}

	var cfg map[string]any

	if err := json.Unmarshal(b, &cfg); err != nil {
		return err //nolint:wrapcheck
	}

	st, _ := cfg["storage"].(map[string]any)
	opts, _ := st["config"].(map[string]any)

	if opts == nil {
		return errors.New("the connection has no storage options")
	}

	opts["path"] = path

	b, err = json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err //nolint:wrapcheck
	}

	return os.WriteFile(cfgPath, b, 0o600) //nolint:wrapcheck,mnd
}

// checkNetworkAndClock waits until the repository's server answers and
// offers to set the clock from its Date header: a clock that is far off
// breaks certificate checks and the request signatures of S3 and others.
func (c *commandImageRecover) checkNetworkAndClock(ctx context.Context, lc *repo.LocalConfig) {
	ep, err := recoverymedia.RepositoryEndpoint(lc)
	if err != nil || ep == nil {
		return
	}

	serverTime, err := c.probe(ctx, ep.URL)
	if err != nil {
		c.out.printStderr("Unable to reach %v: %v\nCheck the network cable. Trying to connect anyway.\n", ep.URL, err)
		return
	}

	now := clock.Now()

	skew := now.Sub(serverTime)
	if skew.Abs() < recoverMaxClockSkew {
		return
	}

	c.out.printStdout("This computer's clock says %v, but %v says %v.\n", now.UTC().Format(time.RFC1123), ep.URL, serverTime.UTC().Format(time.RFC1123))

	set := c.answers != nil && c.answers.SetClock
	if c.answers == nil {
		set, err = c.askYesNo("Set the clock to the server's time? [Y/n] ", true)
		if err != nil {
			return
		}
	}

	if !set {
		return
	}

	if err := recoverymedia.SetSystemClock(serverTime); err != nil {
		c.out.printStderr("%v\n", err)
		return
	}

	c.out.printStdout("Clock set.\n")
}

// probe waits until a server answers and returns the time in its Date header.
// The certificate is not checked: only the time is read, and a clock that is
// far off would fail the check. The repository connection checks it later.
func (c *commandImageRecover) probe(ctx context.Context, url string) (time.Time, error) {
	client := &http.Client{
		Timeout: recoverProbeTimeout,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec
		},
	}

	deadline := clock.Now().Add(c.networkWait)
	waiting := false

	for {
		t, err := probeDate(ctx, client, url)
		if err == nil {
			return t, nil
		}

		if clock.Now().After(deadline) {
			return time.Time{}, err
		}

		if !waiting {
			c.out.printStdout("Waiting for the network...\n")

			waiting = true
		}

		select {
		case <-ctx.Done():
			return time.Time{}, ctx.Err() //nolint:wrapcheck
		case <-time.After(recoverProbeRetry):
		}
	}
}

func probeDate(ctx context.Context, client *http.Client, url string) (time.Time, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, http.NoBody)
	if err != nil {
		return time.Time{}, errors.Wrap(err, "invalid URL")
	}

	resp, err := client.Do(req)
	if err != nil {
		return time.Time{}, err //nolint:wrapcheck
	}

	resp.Body.Close() //nolint:errcheck

	t, err := http.ParseTime(resp.Header.Get("Date"))

	return t, errors.Wrap(err, "the server sent no time")
}

func (c *commandImageRecover) openRepository(ctx context.Context, cfgPath string) (repo.Repository, error) {
	var (
		password string
		err      error
	)

	switch {
	case c.info.Password != "":
		password = c.info.Password
	case c.answers != nil && c.answers.Password != "":
		password = c.answers.Password
	default:
		password, err = c.svc.getPasswordFromFlags(ctx, false, false)
		if err != nil {
			return nil, errors.Wrap(err, "unable to get the repository password")
		}
	}

	rep, err := repo.Open(ctx, cfgPath, password, c.svc.optionsFromFlags(ctx))
	if err != nil {
		return nil, errors.Wrap(err, "unable to open the repository")
	}

	return rep, nil
}

// recoverableImages returns the complete images of every computer, those of
// the preferred sources first, newest first.
func recoverableImages(ctx context.Context, rep repo.Repository, preferred []string) ([]*snapshot.Manifest, error) {
	ids, err := snapshot.ListSnapshotManifests(ctx, rep, nil, map[string]string{image.TagKey: image.TagValue})
	if err != nil {
		return nil, errors.Wrap(err, "unable to list image snapshots")
	}

	mans, err := snapshot.LoadSnapshots(ctx, rep, ids)
	if err != nil {
		return nil, errors.Wrap(err, "unable to load image snapshots")
	}

	mans = slices.DeleteFunc(mans, func(m *snapshot.Manifest) bool { return m.IncompleteReason != "" })

	rank := func(m *snapshot.Manifest) int {
		if i := slices.Index(preferred, m.Source.String()); i >= 0 {
			return i
		}

		return len(preferred)
	}

	slices.SortStableFunc(mans, func(a, b *snapshot.Manifest) int {
		if d := rank(a) - rank(b); d != 0 {
			return d
		}

		return b.StartTime.ToTime().Compare(a.StartTime.ToTime())
	})

	return mans, nil
}

func (c *commandImageRecover) chooseImage(ctx context.Context, rep repo.Repository) (*snapshot.Manifest, error) {
	if c.answers != nil && c.answers.Image != "" {
		return resolveImageSnapshot(ctx, rep, c.answers.Image)
	}

	mans, err := recoverableImages(ctx, rep, c.info.Sources)
	if err != nil {
		return nil, err
	}

	if len(mans) == 0 {
		return nil, errors.New("the repository has no images")
	}

	if c.answers != nil {
		return mans[0], nil
	}

	c.out.printStdout("\nImages, newest first:\n")

	shown := min(len(mans), recoverImagesShown)
	for i, m := range mans[:shown] {
		c.out.printStdout("  %2d. %v  %v  %v\n", i+1, formatTimestamp(m.StartTime.ToTime()), m.Source, m.ID)
	}

	if shown < len(mans) {
		c.out.printStdout("  ... and %v older; enter a snapshot ID to choose one of them.\n", len(mans)-shown)
	}

	for {
		a, err := c.ask("Image to restore [1]: ")
		if err != nil {
			return nil, err
		}

		if a == "" {
			return mans[0], nil
		}

		if n, err := strconv.Atoi(a); err == nil && n >= 1 && n <= shown {
			return mans[n-1], nil
		}

		if m, err := resolveImageSnapshot(ctx, rep, a); err == nil {
			return m, nil
		}

		c.out.printStdout("Enter a number from the list or a snapshot ID.\n")
	}
}

// recoverTarget is the disk or file chosen to restore to.
type recoverTarget struct {
	path string

	// disk is nil for a disk image file.
	disk *imagerestore.DiskInfo
}

func (c *commandImageRecover) chooseTarget(s *imagerestore.Snapshot, repoPath string) (*recoverTarget, error) {
	disks, err := imagerestore.ListDisks()
	if err != nil {
		return nil, errors.Wrap(err, "unable to list the disks")
	}

	keep := []string{repoPath}
	if !imagerestore.InRecoveryEnvironment() {
		keep = append(keep, systemRoot())
	}

	problems := make([]string, len(disks))
	for i := range disks {
		problems[i] = disks[i].TargetProblem(s.Layout.MinTargetSize(), s.Layout.SectorSize, keep...)
	}

	if t := c.chosenTargetPath(); t != "" {
		return namedTarget(t, disks, problems)
	}

	return c.askTarget(s, disks, problems)
}

// namedTarget returns the disk or file named by the answers or --target.
func namedTarget(t string, disks []imagerestore.DiskInfo, problems []string) (*recoverTarget, error) {
	for i := range disks {
		if strings.EqualFold(disks[i].Path, t) {
			if problems[i] != "" {
				return nil, errors.Errorf("%v cannot be restored to: %v", t, problems[i])
			}

			return &recoverTarget{path: disks[i].Path, disk: &disks[i]}, nil
		}
	}

	if strings.HasPrefix(t, `\\.\`) || strings.HasPrefix(t, "/dev/") {
		return nil, errors.Errorf("%v is not a disk of this computer", t)
	}

	return &recoverTarget{path: t}, nil
}

func (c *commandImageRecover) askTarget(s *imagerestore.Snapshot, disks []imagerestore.DiskInfo, problems []string) (*recoverTarget, error) {
	c.out.printStdout("\nDisks (the image needs %v):\n", units.BytesString(s.Layout.MinTargetSize()))

	for i := range disks {
		d := &disks[i]
		state := "holds " + strings.Join(d.DescribeVolumes(), "; ")

		if problems[i] != "" {
			state = "cannot be used: " + problems[i]
		}

		c.out.printStdout("  %2d. %v\n      %v\n", i+1, d.Describe(), state)
	}

	for {
		a, err := c.ask("Disk to restore to (number): ")
		if err != nil {
			return nil, err
		}

		n, err := strconv.Atoi(a)
		if err != nil || n < 1 || n > len(disks) {
			c.out.printStdout("Enter a number from the list.\n")
			continue
		}

		if problems[n-1] != "" {
			c.out.printStdout("That disk cannot be used: %v.\n", problems[n-1])
			continue
		}

		return &recoverTarget{path: disks[n-1].Path, disk: &disks[n-1]}, nil
	}
}

func (c *commandImageRecover) chosenTargetPath() string {
	if c.target != "" {
		return c.target
	}

	if c.answers != nil {
		return c.answers.Target
	}

	return ""
}

func systemRoot() string {
	if isWindows() {
		return os.Getenv("SystemDrive") + `\`
	}

	return "/"
}

// confirmationWord is what a person types to erase a disk: its number on
// Windows, its device name elsewhere.
func confirmationWord(path string) string {
	if n, ok := strings.CutPrefix(strings.ToLower(path), `\\.\physicaldrive`); ok {
		return n
	}

	return filepath.Base(path)
}

func (c *commandImageRecover) confirm(t *recoverTarget) error {
	if c.answers != nil {
		if !c.answers.Confirm {
			return errors.New("the answers do not confirm erasing the target")
		}

		return nil
	}

	if t.disk == nil {
		ok, err := c.askYesNo("Write the image to the file "+t.path+"? [y/N] ", false)
		if err != nil {
			return err
		}

		if !ok {
			return errors.New("canceled")
		}

		return nil
	}

	word := confirmationWord(t.path)

	c.out.printStdout("\nEverything on %v will be erased:\n", t.disk.Describe())

	for _, v := range t.disk.DescribeVolumes() {
		c.out.printStdout("  %v\n", v)
	}

	a, err := c.ask("Type " + word + " to erase it and restore, or anything else to cancel: ")
	if err != nil {
		return err
	}

	if !strings.EqualFold(a, word) {
		return errors.New("canceled; nothing was written")
	}

	return nil
}

func (c *commandImageRecover) restore(ctx context.Context, rep repo.Repository, s *imagerestore.Snapshot, t *recoverTarget) error {
	if err := c.confirm(t); err != nil {
		return err
	}

	if t.disk != nil && len(t.disk.Volumes) > 0 {
		if err := imagerestore.CleanDisk(ctx, t.path); err != nil {
			return errors.Wrap(err, "unable to remove the disk's partitions")
		}
	}

	tgt, err := imagerestore.OpenTarget(t.path, s.Layout.DiskSize, s.Layout.SectorSize)
	if err != nil {
		return errors.Wrap(err, "unable to open the restore target")
	}

	defer tgt.Close() //nolint:errcheck

	if err := c.checkTarget(s, tgt); err != nil {
		return err
	}

	c.out.printStdout("\nRestoring to %v...\n", tgt.Path())

	return restoreImageToTarget(ctx, &c.out, rep, s, tgt, s.Metadata.Volumes, c.parallel)
}

func (c *commandImageRecover) checkTarget(s *imagerestore.Snapshot, t imagerestore.Target) error {
	err := s.CheckTarget(t, false)
	if !errors.Is(err, imagerestore.ErrDuplicateDiskGUID) {
		return err //nolint:wrapcheck
	}

	c.out.printStdout("%v\nWindows takes one of two disks with the same disk GUID offline. Detach the original disk if you can.\n", err)

	force := c.answers != nil && c.answers.Force
	if c.answers == nil {
		force, err = c.askYesNo("Restore anyway? [y/N] ", false)
		if err != nil {
			return err
		}
	}

	if !force {
		return errors.New("canceled; nothing was written")
	}

	return s.CheckTarget(t, true) //nolint:wrapcheck
}

func (c *commandImageRecover) finish(ctx context.Context) error {
	if !imagerestore.InRecoveryEnvironment() {
		return nil
	}

	after := recoverAfterExit
	if c.answers != nil && c.answers.After != "" {
		after = c.answers.After
	}

	if c.answers == nil {
		c.out.printStdout("\nRemove the recovery media before the computer restarts, or it starts the recovery again.\n")

		restart, err := c.askYesNo("Restart now? [Y/n] ", true)
		if err != nil || !restart {
			return nil //nolint:nilerr
		}

		after = recoverAfterReboot
	}

	switch after {
	case recoverAfterReboot, recoverAfterShutdown:
		return recoverymedia.PowerAction(ctx, after) //nolint:wrapcheck
	default:
		return nil
	}
}

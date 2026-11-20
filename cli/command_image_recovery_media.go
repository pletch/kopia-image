package cli

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
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
	"github.com/kopia/kopia/repo/content"
	"github.com/kopia/kopia/snapshot"
	"github.com/kopia/kopia/snapshot/image"
	"github.com/kopia/kopia/snapshot/image/blockdev"
	"github.com/kopia/kopia/snapshot/image/imagerestore"
	"github.com/kopia/kopia/snapshot/image/recoverymedia"
	"github.com/kopia/kopia/snapshot/image/schedule"
	"github.com/kopia/kopia/snapshot/image/securedir"
)

const (
	recoveryCacheDir       = recoverymedia.RecoveryDir + `\cache`
	recoveryMetaCacheBytes = 128 << 20
	minRecoveryUSBBytes    = 2 << 30
	recoveryDriversHost    = "host"
	recoveryDriversNone    = "none"

	mediaFolder = "folder"
	mediaVHDX   = "vhdx"
	mediaUSB    = "usb"
	mediaISO    = "iso"
)

type commandImageRecoveryMedia struct {
	create   commandImageRecoveryMediaCreate
	status   commandImageRecoveryMediaStatus
	register commandImageRecoveryMediaRegister
	kit      commandImageRecoveryMediaKit
}

func (c *commandImageRecoveryMedia) setup(svc advancedAppServices, parent commandParent) {
	cmd := parent.Command("recovery-media", "Build bootable media that restores this computer's images.")
	c.create.setup(svc, cmd)
	c.status.setup(svc, cmd)
	c.register.setup(svc, cmd)
	c.kit.setup(svc, cmd)
}

type commandImageRecoveryMediaCreate struct {
	usb           string
	outputDir     string
	vhdx          string
	iso           string
	connectFrom   string
	embedPassword bool
	caCertFiles   []string
	drivers       string
	addDrivers    []string
	driversFrom   []string
	winre         string
	timeZone      string
	force         bool
	confirmDisk   string
	answersFile   string
	workDir       string
	recordFile    string

	// replacesLegacyRecord is set when the record goes to its default place,
	// so that the one an earlier kopia kept elsewhere is removed.
	replacesLegacyRecord bool

	svc advancedAppServices
	out textOutput
}

func (c *commandImageRecoveryMediaCreate) setup(svc advancedAppServices, parent commandParent) {
	cmd := parent.Command("create", "Build recovery media from this computer's Windows Recovery Environment. Run as administrator.")
	cmd.Flag("usb", `USB disk to erase and make bootable: its number or \\.\PhysicalDriveN (default: choose from a list)`).StringVar(&c.usb)
	cmd.Flag("output-dir", "Write the media's files to this folder, to copy onto any FAT32 USB stick").StringVar(&c.outputDir)
	cmd.Flag("vhdx", "Write a bootable virtual disk for Hyper-V, to test the media").StringVar(&c.vhdx)
	cmd.Flag("iso", "Write an ISO file, to keep and write to a USB stick with a tool such as Rufus when needed; it does not boot as a DVD or in a virtual machine").StringVar(&c.iso)
	cmd.Flag("connect-from", "Repository connection file to put on the media, such as one with read-only access (default: the current one)").StringVar(&c.connectFrom)
	cmd.Flag("embed-password", "Put the repository password on the media; anyone with the media can then read every image").BoolVar(&c.embedPassword)
	cmd.Flag("ca-cert", "PEM file of a certificate authority for the recovery system to trust; repeatable").StringsVar(&c.caCertFiles)
	cmd.Flag("drivers", "host adds this computer's storage, network and USB drivers; none adds none").Default(recoveryDriversHost).EnumVar(&c.drivers, recoveryDriversHost, recoveryDriversNone)
	cmd.Flag("add-driver", "Driver .inf file or folder to add, for other hardware; repeatable").StringsVar(&c.addDrivers)
	cmd.Flag("drivers-from", "Also add the drivers stored with the newest image of this computer, so that the media restores it too, or of every computer with 'all'; repeatable").StringsVar(&c.driversFrom)
	cmd.Flag("winre", "Winre.wim to build from (default: this computer's)").StringVar(&c.winre)
	cmd.Flag("time-zone", "Windows time zone of the recovery system (default: this computer's)").StringVar(&c.timeZone)
	cmd.Flag("force", "Allow a --usb disk that is not attached through USB").BoolVar(&c.force)
	cmd.Flag("confirm-disk", "Confirm erasing the --usb disk with its number instead of typing it").Hidden().StringVar(&c.confirmDisk)
	cmd.Flag("answers", "Answers file that makes the recovery unattended, for tests").Hidden().StringVar(&c.answersFile)
	cmd.Flag("work-dir", "Folder to build in (default: ProgramData\\kopia\\image\\media-build)").Hidden().StringVar(&c.workDir)
	cmd.Flag("record-file", "File recording the media built (default: ProgramData\\kopia\\recovery-media.json)").Hidden().StringVar(&c.recordFile)
	cmd.Action(svc.repositoryReaderAction(c.run))

	c.svc = svc
	c.out.setup(svc)
}

// mediaTarget is where the media is written.
type mediaTarget struct {
	kind string
	path string
	disk *imagerestore.DiskInfo
	num  uint32
}

func (t *mediaTarget) String() string {
	if t.disk != nil {
		return t.disk.Describe()
	}

	return t.path
}

func (c *commandImageRecoveryMediaCreate) run(ctx context.Context, rep repo.Repository) error {
	if !isWindows() {
		return recoverymedia.ErrNotSupported
	}

	defer keepConsoleRunning()()

	if !recoverymedia.IsElevated() {
		return errors.New("building recovery media needs an administrator: run kopia from an elevated prompt")
	}

	if err := c.prepareStateDir(); err != nil {
		return err
	}

	target, err := c.validateTarget(ctx, rep)
	if err != nil {
		return err
	}

	lc, err := c.connection(rep)
	if err != nil {
		return err
	}

	m, err := gatherRecoveryMedia(ctx, rep, lc, c.caCertFiles, c.out.printStderr)
	if err != nil {
		return err
	}

	start := clock.Now()

	repoDrivers, removeDrivers, err := c.repositoryDrivers(ctx, rep)
	defer removeDrivers()

	if err != nil {
		return err
	}

	built, err := c.build(ctx, m, repoDrivers)
	if err != nil {
		return err
	}

	rec := m.record(built, target.String(), c.connectFrom, c.embedPassword)
	rec.Drivers = append(rec.Drivers, addedDriverINFs(c.addDrivers, built.Drivers)...)

	// other computers register the media from this copy
	if err := recoverymedia.WriteRecord(filepath.Join(built.MediaDir, recoverymedia.MediaRecordFile), rec.ForMedia(m.computer)); err != nil {
		return err //nolint:wrapcheck
	}

	c.out.printStdout("Writing the media to %v\n", target)

	if err := c.write(ctx, built, target); err != nil {
		return err
	}

	if err := recoverymedia.WriteRecord(c.recordFile, rec); err != nil {
		return err //nolint:wrapcheck
	}

	if c.replacesLegacyRecord {
		if err := recoverymedia.RemoveLegacyRecord(); err != nil {
			log(ctx).Warnf("%v", err)
		}
	}

	c.printSummary(built, target, clock.Now().Sub(start))

	return nil
}

// prepareStateDir secures the image state folder, where the media is built
// and recorded, unless both are elsewhere, as in tests.
func (c *commandImageRecoveryMediaCreate) prepareStateDir() error {
	if c.workDir != "" && c.recordFile != "" {
		return nil
	}

	stateDir, err := schedule.DefaultConfigDir()
	if err != nil {
		return err //nolint:wrapcheck
	}

	if err := securedir.Secure(filepath.Dir(stateDir), securedir.UsersRead); err != nil {
		return errors.Wrap(err, "unable to secure the image state folder")
	}

	if err := securedir.Secure(stateDir, securedir.AdminsOnly); err != nil {
		return errors.Wrap(err, "unable to secure the image folder")
	}

	if c.workDir == "" {
		c.workDir = filepath.Join(stateDir, "media-build")
	}

	if c.recordFile == "" {
		p, err := recoverymedia.DefaultRecordPath()
		if err != nil {
			return err //nolint:wrapcheck
		}

		c.recordFile = p
		c.replacesLegacyRecord = true
	}

	return nil
}

func (c *commandImageRecoveryMediaCreate) validateTarget(ctx context.Context, rep repo.Repository) (*mediaTarget, error) {
	n := 0

	for _, s := range []string{c.usb, c.outputDir, c.vhdx, c.iso} {
		if s != "" {
			n++
		}
	}

	if n > 1 {
		return nil, errors.New("pass only one of --usb, --output-dir, --vhdx or --iso")
	}

	if n == 0 {
		if err := c.pickTarget(); err != nil {
			return nil, err
		}
	}

	if c.embedPassword && c.iso != "" {
		if err := c.confirmPasswordInISO(); err != nil {
			return nil, err
		}
	}

	switch {
	case c.outputDir != "":
		if entries, err := os.ReadDir(c.outputDir); err == nil && len(entries) > 0 {
			return nil, errors.Errorf("%v is not empty", c.outputDir)
		}

		return &mediaTarget{kind: mediaFolder, path: c.outputDir}, nil
	case c.vhdx != "":
		if !strings.EqualFold(filepath.Ext(c.vhdx), ".vhdx") {
			return nil, errors.Errorf("%v does not end with .vhdx", c.vhdx)
		}

		if _, err := os.Stat(c.vhdx); err == nil {
			return nil, errors.Errorf("%v already exists", c.vhdx)
		}

		return &mediaTarget{kind: mediaVHDX, path: c.vhdx}, nil
	case c.iso != "":
		if !strings.EqualFold(filepath.Ext(c.iso), ".iso") {
			return nil, errors.Errorf("%v does not end with .iso", c.iso)
		}

		if _, err := os.Stat(c.iso); err == nil {
			return nil, errors.Errorf("%v already exists", c.iso)
		}

		return &mediaTarget{kind: mediaISO, path: c.iso}, nil
	default:
		return c.validateUSB(ctx, rep)
	}
}

func (c *commandImageRecoveryMediaCreate) validateUSB(ctx context.Context, rep repo.Repository) (*mediaTarget, error) {
	path := c.usb
	if _, err := strconv.ParseUint(path, 10, 32); err == nil {
		path = `\\.\PhysicalDrive` + path
	}

	disks, err := imagerestore.ListDisks()
	if err != nil {
		return nil, errors.Wrap(err, "unable to list the disks")
	}

	i := slices.IndexFunc(disks, func(d imagerestore.DiskInfo) bool { return strings.EqualFold(d.Path, path) })
	if i < 0 {
		return nil, errors.Errorf("%v is not a disk of this computer", c.usb)
	}

	d := &disks[i]
	word := confirmationWord(d.Path)

	num, err := strconv.ParseUint(word, 10, 32)
	if err != nil {
		return nil, errors.Errorf("invalid disk %v", d.Path)
	}

	if problem := usbProblem(ctx, rep, d, c.force); problem != "" {
		return nil, errors.Errorf("%v cannot be used: %v", d.Describe(), problem)
	}

	c.out.printStdout("Everything on %v will be erased:\n", d.Describe())

	for _, v := range d.DescribeVolumes() {
		c.out.printStdout("  %v\n", v)
	}

	answer := c.confirmDisk
	if answer == "" {
		c.out.printStdout("Type %v to erase it and continue: ", word)

		answer, err = readLine(c.svc)
		if err != nil {
			return nil, err
		}
	}

	if !strings.EqualFold(strings.TrimSpace(answer), word) {
		return nil, errors.New("canceled; nothing was written")
	}

	return &mediaTarget{kind: mediaUSB, path: d.Path, disk: d, num: uint32(num)}, nil
}

// usbProblem says why a disk must not be erased to become recovery media,
// or returns "".
func usbProblem(ctx context.Context, rep repo.Repository, d *imagerestore.DiskInfo, force bool) string {
	switch {
	case d.HasMountPoint(systemRoot()):
		return "it holds Windows"
	case d.Bus != blockdev.BusUSB && !force:
		return "it is attached through " + d.Bus + ", not USB; pass --force if you are sure"
	case d.Size < minRecoveryUSBBytes:
		return "it is smaller than " + units.BytesString(int64(minRecoveryUSBBytes))
	}

	if d.DiskGUID == "" {
		return ""
	}

	srcs, err := snapshot.ListSources(ctx, rep)
	if err != nil {
		return ""
	}

	for _, s := range srcs {
		if guid, err := image.ParseDiskSourcePath(s.Path); err == nil && strings.EqualFold(guid, d.DiskGUID) {
			return "it is imaged as " + s.String()
		}
	}

	return ""
}

func readLine(svc appServices) (string, error) {
	var b [1]byte

	var sb strings.Builder

	for {
		n, err := svc.stdin().Read(b[:])
		if n == 1 {
			if b[0] == '\n' {
				return sb.String(), nil
			}

			sb.WriteByte(b[0])
		}

		if err != nil {
			if sb.Len() > 0 {
				return sb.String(), nil
			}

			return "", errors.Wrap(err, "no answer")
		}
	}
}

// connection returns the repository connection to embed: read-only, with
// a small cache on the recovery system's RAM disk.
func (c *commandImageRecoveryMediaCreate) connection(rep repo.Repository) (*repo.LocalConfig, error) {
	path := c.connectFrom
	if path == "" {
		path = c.svc.repositoryConfigFileName()
	}

	lc, err := repo.LoadConfigFromFile(path)
	if err != nil {
		return nil, errors.Wrapf(err, "unable to read the repository connection %v", path)
	}

	clone := *lc

	if c.connectFrom == "" {
		clone.ClientOptions = rep.ClientOptions()
	}

	clone.ReadOnly = true
	clone.Caching = recoveryCaching()

	if lc.Storage != nil && lc.Storage.Type != "filesystem" && c.connectFrom == "" {
		c.out.printStderr("The media holds the storage credentials of this connection. Prefer credentials that can only read: pass --connect-from with such a connection.\n")
	}

	return &clone, nil
}

// recoveryCaching is the cache of the recovery system, on its small RAM disk
// X:. It caches no data: a restore reads each chunk once, and the cache keeps
// what it read in the last minutes whatever its size, which filled X: and
// failed every later write to the cache. The metadata cache, which holds the
// chunk lists, has a hard limit for the same reason.
func recoveryCaching() *content.CachingOptions {
	return &content.CachingOptions{
		CacheDirectory:              recoveryCacheDir,
		ContentCacheSizeBytes:       0,
		MetadataCacheSizeBytes:      recoveryMetaCacheBytes,
		MetadataCacheSizeLimitBytes: recoveryMetaCacheBytes,
	}
}

// repositoryDrivers extracts the drivers that --drivers-from asks for from
// the images storing them, the newest version of each driver only, and
// returns them with a function that removes them.
func (c *commandImageRecoveryMediaCreate) repositoryDrivers(ctx context.Context, rep repo.Repository) ([]image.DriverSource, func(), error) {
	if len(c.driversFrom) == 0 {
		return nil, func() {}, nil
	}

	inv, err := image.DriverInventory(ctx, rep)
	if err != nil {
		return nil, func() {}, err //nolint:wrapcheck
	}

	hosts, err := selectDriverHosts(inv, c.driversFrom, rep.ClientOptions().Hostname, c.drivers == recoveryDriversHost)
	if err != nil {
		return nil, func() {}, err
	}

	type candidate struct {
		driver image.Driver
		host   string
		man    *snapshot.Manifest
	}

	var (
		pkgs  []image.DriverSource
		cands []candidate
	)

	for _, h := range hosts {
		c.out.printStderr("Adding the drivers of %v, stored with its image of %v.\n", h.Host, formatTimestamp(h.Image.StartTime.ToTime()))

		for _, d := range h.Drivers {
			// Dir is only an index into cands until extracted
			pkgs = append(pkgs, image.DriverSource{Driver: d, Dir: strconv.Itoa(len(cands))})
			cands = append(cands, candidate{d, h.Host, h.Image})
		}
	}

	dir := c.workDir + "-drivers"
	remove := func() { os.RemoveAll(dir) } //nolint:errcheck

	remove()

	if err := os.MkdirAll(dir, 0o700); err != nil { //nolint:mnd
		return nil, remove, errors.Wrap(err, "unable to create a folder for the drivers")
	}

	var out []image.DriverSource

	for _, p := range recoverymedia.NewestPackages(pkgs) {
		i, _ := strconv.Atoi(p.Dir)
		cand := cands[i]
		dest := filepath.Join(dir, strconv.Itoa(i)+"-"+cand.driver.Entry)

		if err := image.ExtractDriver(ctx, rep, cand.man, cand.driver, dest); err != nil {
			c.out.printStderr("Unable to read the driver %v of %v: %v\n", cand.driver, cand.host, err)
			continue
		}

		out = append(out, image.DriverSource{Driver: cand.driver, Dir: dest})
	}

	return out, remove, nil
}

// selectDriverHosts returns the computers of the inventory that names lists,
// or all of them for "all". This computer is left out when its own drivers
// are added from its driver store, unless it is named.
func selectDriverHosts(inv []image.HostDrivers, names []string, thisHost string, hostDrivers bool) ([]image.HostDrivers, error) {
	if slices.ContainsFunc(names, func(n string) bool { return strings.EqualFold(n, "all") }) {
		if len(inv) == 0 {
			return nil, errors.New("no image stores drivers yet: create images with this version of kopia first")
		}

		return slices.DeleteFunc(slices.Clone(inv), func(h image.HostDrivers) bool {
			return hostDrivers && strings.EqualFold(h.Host, thisHost)
		}), nil
	}

	var out []image.HostDrivers

	for _, n := range names {
		i := slices.IndexFunc(inv, func(h image.HostDrivers) bool { return strings.EqualFold(h.Host, n) })
		if i < 0 {
			return nil, errors.Errorf("no image of %v stores drivers: create an image on %v with this version of kopia first", n, n)
		}

		if !slices.ContainsFunc(out, func(h image.HostDrivers) bool { return strings.EqualFold(h.Host, n) }) {
			out = append(out, inv[i])
		}
	}

	return out, nil
}

// addedDriverINFs identifies the drivers added from --add-driver files and
// folders that were added, by their INF names, so that the record tells the
// media has them.
func addedDriverINFs(paths, added []string) []image.Driver {
	var out []image.Driver

	for _, p := range paths {
		if !slices.Contains(added, p) {
			continue
		}

		filepath.WalkDir(p, func(path string, d os.DirEntry, err error) error { //nolint:errcheck
			if err == nil && !d.IsDir() && strings.EqualFold(filepath.Ext(path), ".inf") {
				out = append(out, image.Driver{INF: filepath.Base(path)})
			}

			return nil
		})
	}

	return out
}

func (c *commandImageRecoveryMediaCreate) build(ctx context.Context, m *recoveryMedia, repoDrivers []image.DriverSource) (*recoverymedia.BuildResult, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, errors.Wrap(err, "unable to find the kopia executable")
	}

	cfg, err := json.MarshalIndent(m.conn, "", "  ")
	if err != nil {
		return nil, errors.Wrap(err, "unable to encode the repository connection")
	}

	info := m.info()

	if c.embedPassword {
		info.Password, err = c.svc.getPasswordFromFlags(ctx, false, true)
		if err != nil {
			return nil, errors.Wrap(err, "unable to get the repository password")
		}
	}

	var answers []byte

	if c.answersFile != "" {
		if answers, err = os.ReadFile(c.answersFile); err != nil {
			return nil, errors.Wrap(err, "unable to read the answers")
		}
	}

	opt := &recoverymedia.BuildOptions{
		WorkDir:      c.workDir,
		WinRE:        c.winre,
		KopiaExe:     exe,
		Config:       cfg,
		Info:         info,
		Kit:          recoverymedia.RenderKit(m.kit(c.embedPassword)),
		Answers:      answers,
		HostDrivers:  c.drivers == recoveryDriversHost,
		ExtraDrivers: c.addDrivers,
		TimeZone:     c.timeZone,
		Logf:         c.out.printStderr,
	}

	opt.RepositoryDrivers = repoDrivers

	built, err := recoverymedia.Build(ctx, opt)
	if err != nil {
		os.RemoveAll(c.workDir) //nolint:errcheck

		return nil, errors.Wrap(err, "unable to build the recovery media")
	}

	return built, nil
}

func (c *commandImageRecoveryMediaCreate) write(ctx context.Context, built *recoverymedia.BuildResult, t *mediaTarget) error {
	// the build holds the repository connection; remove it once written
	defer os.RemoveAll(c.workDir) //nolint:errcheck

	var err error

	switch t.kind {
	case mediaFolder:
		err = recoverymedia.WriteFolder(built.MediaDir, t.path)
	case mediaVHDX:
		err = recoverymedia.WriteVHDX(ctx, built.MediaDir, t.path, c.workDir)
	case mediaISO:
		err = recoverymedia.WriteISO(ctx, built.MediaDir, t.path)
	default:
		err = recoverymedia.WriteUSB(ctx, built.MediaDir, t.num, t.disk.Size, c.workDir)
	}

	return errors.Wrapf(err, "unable to write the media to %v", t)
}

func (c *commandImageRecoveryMediaCreate) printSummary(built *recoverymedia.BuildResult, t *mediaTarget, took time.Duration) {
	c.out.printStdout("Built recovery media (%v) on %v in %v from %v.\n", units.BytesString(built.Size), t, took.Truncate(time.Second), built.WinRE)
	c.out.printStdout("  time zone %v, %v drivers added\n", built.TimeZone, len(built.Drivers))

	for _, f := range built.DriverFailures {
		c.out.printStdout("  driver not added: %v\n", f)
	}

	switch t.kind {
	case mediaFolder:
		c.out.printStdout("Copy the folder's contents to a FAT32 USB stick. ")
	case mediaISO:
		c.out.printStdout("Write the ISO file to a USB stick with a tool such as Rufus when needed, and keep a copy away from the computers it restores. It does not boot as a DVD or in a virtual machine: test the media with --vhdx. ")
	}

	c.out.printStdout("Boot from the media to test it, and keep %v with it.\n", recoverymedia.KitFileName)

	if c.embedPassword {
		c.out.printStdout("The media holds the repository password: keep it as safe as the password itself.\n")
	}
}

// recoveryMedia is what a computer's recovery media holds.
type recoveryMedia struct {
	conn     *repo.LocalConfig
	caCerts  []string
	disks    []recoverymedia.KitDisk
	userHost string
	computer string
	now      time.Time
}

// gatherRecoveryMedia collects what the media and kit describe: the
// connection, the certificate authorities to trust and this computer's
// images.
func gatherRecoveryMedia(ctx context.Context, rep repo.Repository, conn *repo.LocalConfig, caFiles []string, warnf func(string, ...any)) (*recoveryMedia, error) {
	co := rep.ClientOptions()
	m := &recoveryMedia{conn: conn, userHost: co.UsernameAtHost(), now: clock.Now()}

	m.computer, _ = os.Hostname()

	var err error

	if m.caCerts, err = recoveryCACerts(ctx, conn, caFiles, warnf); err != nil {
		return nil, err
	}

	if m.disks, err = thisComputersDisks(ctx, rep); err != nil {
		return nil, err
	}

	if len(m.disks) == 0 {
		warnf("The repository has no images of %v yet: create one with 'kopia image create C:'.\n", m.userHost)
	}

	return m, nil
}

// recoveryCACerts returns the certificate authorities the recovery system
// must trust: those given, and the root that verifies the repository's
// server now, unless the connection pins a certificate or carries its own
// authority.
func recoveryCACerts(ctx context.Context, conn *repo.LocalConfig, caFiles []string, warnf func(string, ...any)) ([]string, error) {
	var out []string

	for _, f := range caFiles {
		b, err := os.ReadFile(f) //nolint:gosec
		if err != nil {
			return nil, errors.Wrap(err, "unable to read a certificate authority")
		}

		if _, err := recoverymedia.ParseCertificates(string(b)); err != nil {
			return nil, errors.Wrapf(err, "invalid certificate authority %v", f)
		}

		out = append(out, string(b))
	}

	ep, err := recoverymedia.RepositoryEndpoint(conn)
	if err != nil || ep == nil || !strings.HasPrefix(ep.URL, "https://") || ep.Pinned || ep.HasOwnCA {
		return out, nil //nolint:nilerr
	}

	root, err := recoverymedia.ServerRoot(ctx, ep.URL)
	if err != nil {
		warnf("Unable to read the certificate authority of %v: %v\n", ep.URL, err)
		return out, nil
	}

	if !slices.Contains(out, root) {
		out = append(out, root)
	}

	return out, nil
}

// thisComputersDisks returns the image sources of this computer with the
// partition table of their newest image.
func thisComputersDisks(ctx context.Context, rep repo.Repository) ([]recoverymedia.KitDisk, error) {
	co := rep.ClientOptions()

	mans, err := recoverableImages(ctx, rep, nil)
	if err != nil {
		return nil, err
	}

	var disks []recoverymedia.KitDisk

	seen := map[string]bool{}

	for _, man := range mans {
		src := man.Source.String()
		if man.Source.Host != co.Hostname || man.Source.UserName != co.Username || seen[src] {
			continue
		}

		seen[src] = true

		d := recoverymedia.KitDisk{Source: src, LatestImage: string(man.ID) + " of " + formatTimestamp(man.StartTime.ToTime())}

		if s, err := imagerestore.LoadSnapshot(ctx, rep, man); err == nil {
			d.Layout = s.Layout
		}

		disks = append(disks, d)
	}

	return disks, nil
}

func (m *recoveryMedia) sources() []string {
	out := make([]string, 0, len(m.disks))
	for _, d := range m.disks {
		out = append(out, d.Source)
	}

	return out
}

func (m *recoveryMedia) info() *recoverymedia.Info {
	return &recoverymedia.Info{
		BuiltAt:      m.now,
		KopiaVersion: repo.BuildVersion,
		Computer:     m.computer,
		Sources:      m.sources(),
		Repository:   recoverymedia.DescribeRepository(m.conn),
		CACerts:      m.caCerts,
	}
}

func (m *recoveryMedia) kit(passwordEmbedded bool) *recoverymedia.Kit {
	return &recoverymedia.Kit{
		Computer:         m.computer,
		BuiltAt:          m.now,
		KopiaVersion:     repo.BuildVersion,
		Connection:       m.conn,
		UserHost:         m.userHost,
		Disks:            m.disks,
		CACerts:          m.caCerts,
		PasswordEmbedded: passwordEmbedded,
	}
}

func (m *recoveryMedia) record(built *recoverymedia.BuildResult, target, connectFrom string, passwordEmbedded bool) *recoverymedia.Record {
	r := &recoverymedia.Record{
		BuiltAt:          m.now,
		KopiaVersion:     repo.BuildVersion,
		Target:           target,
		WinRE:            built.WinRE,
		WinREDigest:      built.WinREDigest,
		ConnectFrom:      connectFrom,
		ConnectionDigest: recoverymedia.ConnectionDigest(m.conn),
		CACerts:          m.caCerts,
		Disks:            map[string]string{},
		PasswordEmbedded: passwordEmbedded,
		Repository:       recoverymedia.DescribeRepository(m.conn),
		Drivers:          built.DriverPackages,
	}

	for _, d := range m.disks {
		if d.Layout != nil {
			r.Disks[d.Source] = recoverymedia.LayoutDigest(d.Layout)
		}
	}

	return r
}

type commandImageRecoveryMediaStatus struct {
	recordFile string

	svc advancedAppServices
	out textOutput
}

// recoveryMediaAdvice says what to do about media that no longer matches.
func recoveryMediaAdvice(r *recoverymedia.Record) string {
	if r.Shared() {
		return fmt.Sprintf("Build new media on %v and register it with 'kopia image recovery-media register', or build media here with 'kopia image recovery-media create'", r.BuiltOn)
	}

	return "Build new media with 'kopia image recovery-media create'"
}

// commandImageRecoveryMediaRegister records media built on another computer
// as this computer's recovery media, so that status and the Images page check
// it instead of reporting that this computer has none.
type commandImageRecoveryMediaRegister struct {
	media      string
	force      bool
	recordFile string

	svc advancedAppServices
	out textOutput
}

func (c *commandImageRecoveryMediaRegister) setup(svc advancedAppServices, parent commandParent) {
	cmd := parent.Command("register", "Use recovery media built on another computer for this one, so that status and the Images page check it. Run as administrator.")
	cmd.Arg("media", `The media's drive or folder, such as E:\, or its `+recoverymedia.MediaRecordFile).Required().StringVar(&c.media)
	cmd.Flag("force", "Register even though this computer reaches the repository differently than the media").BoolVar(&c.force)
	cmd.Flag("record-file", "File recording the media (default: ProgramData\\kopia\\recovery-media.json)").Hidden().StringVar(&c.recordFile)
	cmd.Action(svc.repositoryReaderAction(c.run))

	c.svc = svc
	c.out.setup(svc)
}

func (c *commandImageRecoveryMediaRegister) run(ctx context.Context, rep repo.Repository) error {
	if !isWindows() {
		return recoverymedia.ErrNotSupported
	}

	recordFile, replacesLegacy, err := c.recordPath()
	if err != nil {
		return err
	}

	r, err := readMediaRecord(c.media)
	if err != nil {
		return err
	}

	if host, _ := os.Hostname(); strings.EqualFold(r.BuiltOn, host) {
		return errors.New("this media was built on this computer, which recorded it when building it")
	}

	lc, err := repo.LoadConfigFromFile(c.svc.repositoryConfigFileName())
	if err != nil {
		return errors.Wrap(err, "unable to read this computer's repository connection")
	}

	local := recoverymedia.DescribeRepository(lc)
	if r.Repository != local {
		if !c.force {
			return errors.Errorf("the media leads to %v, but this computer uses %v; if both are the same repository, register with --force", r.Repository, local)
		}

		c.out.printStderr("The media leads to %v; this computer uses %v.\n", r.Repository, local)
	}

	// later checks compare with how this computer reaches the repository now
	r.Repository = local
	r.Target = c.media

	if err := recoverymedia.WriteRecord(recordFile, r); err != nil {
		return err //nolint:wrapcheck
	}

	if replacesLegacy {
		if err := recoverymedia.RemoveLegacyRecord(); err != nil {
			log(ctx).Warnf("%v", err)
		}
	}

	c.out.printStdout("Registered the recovery media built on %v %v with kopia %v as this computer's.\n", r.BuiltOn, formatTimestamp(r.BuiltAt), r.KopiaVersion)

	if r.Drivers == nil {
		c.out.printStdout("It has the storage and network drivers of %v. ", r.BuiltOn)
	} else {
		c.out.printStdout("It has %v storage, network and USB drivers, added on %v. ", len(r.Drivers), r.BuiltOn)
	}

	c.out.printStdout("Check that it works here: boot this computer from it, or restore one of its images to a file with 'kopia image recover --target <file>'.\n")

	for _, f := range recoveryMediaFindings(ctx, rep, r, c.svc.repositoryConfigFileName(), nil) {
		c.out.printStdout("  - %v\n", f)
	}

	return nil
}

// recordPath returns where to record the media, and whether that is the
// default place, which needs an administrator.
func (c *commandImageRecoveryMediaRegister) recordPath() (path string, isDefault bool, err error) {
	if c.recordFile != "" {
		return c.recordFile, false, nil
	}

	if !recoverymedia.IsElevated() {
		return "", false, errors.New("registering recovery media needs an administrator: run kopia from an elevated prompt")
	}

	stateDir, err := schedule.StateDir()
	if err != nil {
		return "", false, err //nolint:wrapcheck
	}

	if err := securedir.Secure(stateDir, securedir.UsersRead); err != nil {
		return "", false, errors.Wrap(err, "unable to secure the image state folder")
	}

	p, err := recoverymedia.DefaultRecordPath()

	return p, true, err //nolint:wrapcheck
}

// readMediaRecord reads the record that "kopia image recovery-media create"
// put on media: from the media's root when given a drive or folder.
func readMediaRecord(media string) (*recoverymedia.Record, error) {
	p := media
	if fi, err := os.Stat(media); err == nil && fi.IsDir() {
		p = filepath.Join(media, recoverymedia.MediaRecordFile)
	}

	r, err := recoverymedia.ReadRecord(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, errors.Errorf("%v has no %v: build the media again with this version of kopia, which puts it there", media, recoverymedia.MediaRecordFile)
	}

	if err != nil {
		return nil, err //nolint:wrapcheck
	}

	if !r.Shared() {
		return nil, errors.Errorf("%v does not name the computer that built the media", p)
	}

	return r, nil
}

func (c *commandImageRecoveryMediaStatus) setup(svc advancedAppServices, parent commandParent) {
	cmd := parent.Command("status", "Say whether the last recovery media built still matches this computer.")
	cmd.Flag("record-file", "File recording the media (default: ProgramData\\kopia\\recovery-media.json)").Hidden().StringVar(&c.recordFile)
	cmd.Action(svc.repositoryReaderAction(c.run))

	c.svc = svc
	c.out.setup(svc)
}

func (c *commandImageRecoveryMediaStatus) run(ctx context.Context, rep repo.Repository) error {
	if !isWindows() {
		return recoverymedia.ErrNotSupported
	}

	var (
		r   *recoverymedia.Record
		err error
	)

	if c.recordFile != "" {
		r, err = recoverymedia.ReadRecord(c.recordFile)
	} else {
		r, err = recoverymedia.ReadDefaultRecord()
	}

	if errors.Is(err, os.ErrNotExist) {
		c.out.printStdout("No recovery media was built on this computer. Build it with 'kopia image recovery-media create', or register media built on another computer with 'kopia image recovery-media register'.\n")
		return nil
	}

	if err != nil {
		return err //nolint:wrapcheck
	}

	if r.Shared() {
		c.out.printStdout("Recovery media built on %v %v with kopia %v, registered from %v.\n", r.BuiltOn, formatTimestamp(r.BuiltAt), r.KopiaVersion, r.Target)
	} else {
		c.out.printStdout("Recovery media built %v with kopia %v, written to %v.\n", formatTimestamp(r.BuiltAt), r.KopiaVersion, r.Target)
	}

	findings := recoveryMediaFindings(ctx, rep, r, c.svc.repositoryConfigFileName(), nil)
	if len(findings) == 0 {
		c.out.printStdout("It matches this computer.\n")
		return nil
	}

	c.out.printStdout("%v:\n", recoveryMediaAdvice(r))

	for _, f := range findings {
		c.out.printStdout("  - %v\n", f)
	}

	return nil
}

// recoveryMediaFindings compares recorded media with this computer. layouts
// are the disks just imaged; when nil, the layouts of the newest images of
// this computer are used.
func recoveryMediaFindings(ctx context.Context, rep repo.Repository, r *recoverymedia.Record, configFile string, layouts map[string]*image.Layout) []string {
	cur := &recoverymedia.Current{Now: clock.Now(), KopiaVersion: repo.BuildVersion, Layouts: layouts}

	connFile := configFile
	if r.ConnectFrom != "" {
		connFile = r.ConnectFrom
	}

	var findings []string

	if lc, err := repo.LoadConfigFromFile(connFile); err == nil {
		cur.Connection = lc
	} else {
		findings = append(findings, "unable to read the repository connection "+connFile+": "+err.Error())
	}

	if recoverymedia.IsElevated() {
		if p, err := recoverymedia.LocateWinRE(ctx); err == nil {
			if d, err := recoverymedia.FileDigest(p); err == nil {
				cur.WinREDigest = hex.EncodeToString(d)
			}
		}
	}

	cur.Drivers = thisComputersDrivers(ctx)

	if cur.Layouts == nil {
		cur.Layouts = map[string]*image.Layout{}

		if disks, err := thisComputersDisks(ctx, rep); err == nil {
			for _, d := range disks {
				if d.Layout != nil {
					cur.Layouts[d.Source] = d.Layout
				}
			}
		}
	}

	return append(findings, r.Stale(ctx, cur)...)
}

// thisComputersDrivers returns this computer's storage, network and USB
// drivers, or nil when they cannot be listed.
func thisComputersDrivers(ctx context.Context) []image.Driver {
	ds, err := recoverymedia.HostDrivers(ctx)
	if err != nil {
		if !errors.Is(err, recoverymedia.ErrNotSupported) {
			log(ctx).Debugf("%v", err)
		}

		return nil
	}

	out := make([]image.Driver, 0, len(ds))
	for i := range ds {
		out = append(out, ds[i].Image())
	}

	return out
}

type commandImageRecoveryMediaKit struct {
	output string

	svc advancedAppServices
	out textOutput
}

func (c *commandImageRecoveryMediaKit) setup(svc advancedAppServices, parent commandParent) {
	cmd := parent.Command("kit", "Print the recovery kit: what someone needs to restore this computer, without the password.")
	cmd.Flag("output", "Write the kit to this file").StringVar(&c.output)
	cmd.Action(svc.repositoryReaderAction(c.run))

	c.svc = svc
	c.out.setup(svc)
}

func (c *commandImageRecoveryMediaKit) run(ctx context.Context, rep repo.Repository) error {
	lc, err := repo.LoadConfigFromFile(c.svc.repositoryConfigFileName())
	if err != nil {
		return errors.Wrap(err, "unable to read the repository connection")
	}

	m, err := gatherRecoveryMedia(ctx, rep, lc, nil, c.out.printStderr)
	if err != nil {
		return err
	}

	kit := recoverymedia.RenderKit(m.kit(false))

	if c.output == "" {
		c.out.printStdout("%v", strings.ReplaceAll(kit, "\r\n", "\n"))
		return nil
	}

	return errors.Wrap(os.WriteFile(c.output, []byte(kit), 0o600), "unable to write the kit") //nolint:mnd
}

// pickTarget asks where to write the media when no target is given: to a
// USB disk, which it erases, or to an ISO file to write to one later. It
// sets c.usb or c.iso.
func (c *commandImageRecoveryMediaCreate) pickTarget() error {
	disks, err := imagerestore.ListDisks()
	if err != nil {
		return errors.Wrap(err, "unable to list the disks")
	}

	disks = slices.DeleteFunc(disks, func(d imagerestore.DiskInfo) bool {
		return d.Bus != blockdev.BusUSB || d.HasMountPoint(systemRoot()) || d.Size < minRecoveryUSBBytes
	})

	if len(disks) == 0 {
		c.out.printStdout("No USB disk of %v or more is attached.\n", units.BytesString(int64(minRecoveryUSBBytes)))
	} else {
		c.out.printStdout("USB disks:\n")
	}

	for i := range disks {
		c.out.printStdout("  %v. %v\n     holds %v\n", i+1, disks[i].Describe(), strings.Join(disks[i].DescribeVolumes(), "; "))
	}

	c.out.printStdout("  i. Save an ISO file instead, to write to a USB stick with a tool such as Rufus when needed\n")

	for {
		if len(disks) == 0 {
			c.out.printStdout("Type i to save an ISO file, or q to quit: ")
		} else {
			c.out.printStdout("USB disk to make recovery media, which erases it (number from the list), i for an ISO file, or q to quit: ")
		}

		a, err := readLine(c.svc)
		if err != nil {
			return err
		}

		a = strings.TrimSpace(a)

		switch {
		case strings.EqualFold(a, "q"):
			return errors.New("canceled; nothing was written")
		case strings.EqualFold(a, "i"):
			return c.pickISO()
		}

		if n, err := strconv.Atoi(a); err == nil && n >= 1 && n <= len(disks) {
			c.usb = disks[n-1].Path
			return nil
		}
	}
}

// pickISO asks where to save the ISO file.
func (c *commandImageRecoveryMediaCreate) pickISO() error {
	def := ""
	if home, err := os.UserHomeDir(); err == nil {
		computer, _ := os.Hostname()
		def = filepath.Join(home, "Documents", "kopia-recovery-"+computer+"-"+clock.Now().Format("2006-01-02")+".iso")
	}

	c.out.printStdout("ISO file to write [%v]: ", def)

	a, err := readLine(c.svc)
	if err != nil {
		return err
	}

	c.iso = strings.Trim(strings.TrimSpace(a), `"`)
	if c.iso == "" {
		c.iso = def
	}

	if c.iso == "" {
		return errors.New("no ISO file given")
	}

	return nil
}

// confirmPasswordInISO asks before putting the password in an ISO file,
// which is easily copied and synchronized to other places.
func (c *commandImageRecoveryMediaCreate) confirmPasswordInISO() error {
	c.out.printStdout("--embed-password puts the repository password in the ISO file: anyone with a copy of the file can read every image. Type yes to continue: ")

	a, err := readLine(c.svc)
	if err != nil {
		return err
	}

	if !strings.EqualFold(strings.TrimSpace(a), "yes") {
		return errors.New("canceled; nothing was written")
	}

	return nil
}

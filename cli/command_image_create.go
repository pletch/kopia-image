package cli

import (
	"cmp"
	"context"
	stderrors "errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/pkg/errors"

	"github.com/kopia/kopia/internal/background"
	"github.com/kopia/kopia/internal/clock"
	"github.com/kopia/kopia/internal/units"
	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/compression"
	"github.com/kopia/kopia/snapshot"
	"github.com/kopia/kopia/snapshot/image"
	"github.com/kopia/kopia/snapshot/image/blockdev"
	"github.com/kopia/kopia/snapshot/image/disksource"
	"github.com/kopia/kopia/snapshot/image/recoverymedia"
	"github.com/kopia/kopia/snapshot/policy"
)

const (
	defaultImageFlushInterval = 10 * time.Minute

	// defaultImageCompressor compresses image chunks unless the image
	// source's own policy chooses a compressor: partly used chunks are mostly
	// zeroed free space, which would otherwise be uploaded uncompressed. The
	// inherited default is "none", which every repository stores as its
	// global policy, so it does not express a choice about images.
	defaultImageCompressor compression.Name = "zstd-fastest"
)

// imageCompressor returns the image chunk compressor for a policy compressor
// and whether the image source's own policy set it. "none" means no
// compression.
func imageCompressor(policyCompressor compression.Name, setOnSource bool) compression.Name {
	switch {
	case !setOnSource:
		return defaultImageCompressor
	case policyCompressor == "none":
		return ""
	default:
		return policyCompressor
	}
}

type commandImageCreate struct {
	volumes   []string
	parallel  int
	chunkSize string

	// runStart, doneDisks and doneImages carry a run over attempts made
	// after the connection to the repository was lost; retriesLeft counts
	// the attempts that remain.
	runStart      time.Time
	doneDisks     map[uint32]bool
	doneImages    []disksource.RunImage
	retriesLeft   int
	flushInterval time.Duration
	dryRun        bool
	description   string
	statePath     string
	drivers       bool
	excludeCaches bool
	excludeFNTS   bool
	excludeFNTB   bool
	excludeCommon bool

	// waitForOtherRun makes the run wait for one that is going, rather
	// than fail: scheduled runs of disks on different schedules can meet.
	waitForOtherRun bool

	// req records the outcome of a run that KopiaUI started without a
	// window; loc tells where.
	req imageScheduleRequest
	loc imageScheduleLocation

	progress image.Progress
	uploaded atomic.Int64

	svc appServices
	out textOutput
}

func (c *commandImageCreate) setup(svc appServices, parent commandParent) {
	cmd := parent.Command("create", "Create block-level images of the disks holding the given volumes. Windows only; run as administrator or SYSTEM.")
	cmd.Arg("volume", "A volume on each disk to image, e.g. C:").Required().StringsVar(&c.volumes)
	cmd.Flag("parallel", "Number of chunks hashed and uploaded in parallel; by default as many as the computer has processors, or 64 MiB of chunks if that is more").Default("0").IntVar(&c.parallel)
	cmd.Flag("chunk-size", "Size of the image chunks: a chunk is stored again when any of its bytes changes, "+
		"so smaller chunks store less new data per image, but take more index entries and requests").
		Default(imageChunkSizeDefault).EnumVar(&c.chunkSize, imageChunkSizeNames()...)
	cmd.Flag("flush-interval", "How often to save upload progress, so that a failed run's uploads are reused").Default(defaultImageFlushInterval.String()).DurationVar(&c.flushInterval)
	cmd.Flag("dry-run", "Print the partition plan and the size of the zeroed space without imaging").BoolVar(&c.dryRun)
	cmd.Flag("description", "Free-form snapshot description").StringVar(&c.description)
	cmd.Flag("drivers", "Store this computer's storage, network and USB drivers with the images, for recovery media built on other computers").Default("true").BoolVar(&c.drivers)
	cmd.Flag("exclude-kopia-caches", "Read the files of kopia's caches as zeros, so that their changes do not enlarge images; after a restore, kopia refetches what it needs").Default("true").BoolVar(&c.excludeCaches)
	cmd.Flag("exclude-files-not-to-snapshot", "Read the files that Windows and programs leave out of backup shadow copies (FilesNotToSnapshot, e.g. Outlook mailbox caches) as zeros; a restore deletes them").Default("true").BoolVar(&c.excludeFNTS)
	cmd.Flag("exclude-files-not-to-backup", "Read temporary files, crash reports and dumps, the BITS queue and event trace buffers as zeros (the safe part of FilesNotToBackup); a restore deletes them").Default("true").BoolVar(&c.excludeFNTB)
	cmd.Flag("exclude-common-caches", "Read the caches of web browsers, Windows Update's downloads and Delivery Optimization as zeros; a restore deletes them").Default("true").BoolVar(&c.excludeCommon)
	cmd.Flag("state-file", "Override the file that records shadow copies to remove after a crash").Hidden().StringVar(&c.statePath)
	cmd.Flag("wait-for-other-run", "Wait for an image run that is going to end, rather than fail").BoolVar(&c.waitForOtherRun)
	c.req.flag(cmd)
	cmd.Action(svc.noRepositoryAction(c.req.recording("create", &c.loc, c.runWithWriter)))
	c.svc = svc
	c.out.setup(svc)
}

// runWithWriter opens the repository and a write session itself, rather than
// through a repository action, so that a request records a repository that
// does not open, and counts the bytes the session uploads for the progress.
func (c *commandImageCreate) runWithWriter(ctx context.Context) (err error) {
	defer keepConsoleRunning()()

	c.loc.statePath = c.statePath

	// a dry run writes nothing, so it does not need the repository: it
	// shows what would be imaged, and left out, on any computer
	if c.dryRun {
		return c.run(ctx, nil)
	}

	c.runStart = clock.Now()
	c.doneDisks = map[uint32]bool{}

	// a broken connection to a repository server, as when the computer
	// changes networks, ends the session; the run starts again over a new
	// connection, without the disks it finished
	for attempt := 0; ; attempt++ {
		c.retriesLeft = len(imageConnectionRetryDelays) - attempt

		if c.waitForOtherRun {
			if werr := c.waitForRunToEnd(ctx); werr != nil {
				return werr
			}
		}

		err = c.runOnce(ctx)

		// another run started between the wait and this one's start
		if c.waitForOtherRun && errors.Is(err, disksource.ErrAlreadyRunning) && ctx.Err() == nil {
			attempt--

			continue
		}

		if err == nil || !c.willRetry(err) || ctx.Err() != nil {
			return err
		}

		delay := imageConnectionRetryDelays[attempt]

		log(ctx).Warnf("The connection to the repository was lost: %v. Imaging again in %v; what was saved is not uploaded again, and disks already imaged are not imaged again.", err, delay)

		t := time.NewTimer(delay)

		select {
		case <-ctx.Done():
			t.Stop()
			return err
		case <-t.C:
		}
	}
}

// imageConnectionRetryDelays are the waits before imaging again when the
// connection to the repository broke: a computer that changed networks or a
// server that restarted is usually back within minutes.
//
//nolint:gochecknoglobals
var imageConnectionRetryDelays = []time.Duration{30 * time.Second, 2 * time.Minute, 5 * time.Minute}

// willRetry reports whether a run that failed with err is attempted again.
func (c *commandImageCreate) willRetry(err error) bool {
	return c.retriesLeft > 0 && repo.IsConnectionLost(err) &&
		!errors.Is(err, errImageStopped) && !errors.Is(err, disksource.ErrAlreadyRunning)
}

// imageRunWaitInterval is how often a run waiting for another checks
// whether it has ended.
//
//nolint:gochecknoglobals
var imageRunWaitInterval = 30 * time.Second

// waitForRunToEnd waits while another image run holds the state lock. It
// tests the lock without opening the repository, so a long wait holds no
// connection.
func (c *commandImageCreate) waitForRunToEnd(ctx context.Context) error {
	statePath := c.statePath
	if statePath == "" {
		p, err := disksource.DefaultStatePath()
		if err != nil {
			return errors.Wrap(err, "unable to find the image state file")
		}

		statePath = p
	}

	logged := false

	for {
		running, err := disksource.IsRunning(statePath)
		if err != nil {
			return err //nolint:wrapcheck
		}

		if !running {
			// the run starts now, not when it began to wait
			if logged && len(c.doneImages) == 0 {
				c.runStart = clock.Now()
			}

			return nil
		}

		if !logged {
			log(ctx).Infof("Another image run is going; waiting for it to end.")

			logged = true
		}

		t := time.NewTimer(imageRunWaitInterval)

		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err() //nolint:wrapcheck
		case <-t.C:
		}
	}
}

// runOnce opens the repository and images the disks in one write session.
func (c *commandImageCreate) runOnce(ctx context.Context) (err error) {
	rep, err := c.svc.openRepository(ctx, true)
	if err != nil {
		return errors.Wrap(err, "open repository")
	}

	defer func() {
		if cerr := rep.Close(ctx); cerr != nil {
			err = stderrors.Join(err, errors.Wrap(cerr, "unable to close repository"))
		}
	}()

	return repo.WriteSession(ctx, rep, repo.WriteSessionOptions{ //nolint:wrapcheck
		Purpose:  "cli:image create",
		OnUpload: func(n int64) { c.uploaded.Add(n) },
	}, c.run)
}

func (c *commandImageCreate) run(ctx context.Context, rep repo.RepositoryWriter) (err error) {
	// an image reads the whole used part of each disk: it must yield to what
	// the person is doing
	leave, berr := background.Enter()
	if berr != nil {
		log(ctx).Warnf("%v", berr)
	}

	defer leave()

	statePath := c.statePath
	if statePath == "" {
		if statePath, err = disksource.DefaultStatePath(); err != nil {
			return errors.Wrap(err, "unable to find the image state file")
		}
	}

	// Also for a dry run: it takes and removes shadow copies like any other
	// run, and first removes the leftover ones that the state file lists, so
	// it must be able to trust that file just the same.
	if err := disksource.PrepareStateDir(statePath); err != nil {
		return err //nolint:wrapcheck
	}

	// an attempt after a lost connection continues the run
	start := c.runStart
	if start.IsZero() {
		start = clock.Now()
	}

	status := &disksource.RunStatus{Volumes: c.volumes, StartTime: start, Images: slices.Clone(c.doneImages)}

	if !c.dryRun {
		defer func() {
			// the status file belongs to the run that holds the lock, and
			// a run attempted again records its outcome when it ends
			if errors.Is(err, disksource.ErrAlreadyRunning) || c.willRetry(err) {
				return
			}

			status.EndTime = clock.Now()
			status.Success = err == nil && !status.Stopped

			switch {
			case err != nil:
				status.Error = err.Error()
			case status.Stopped:
				status.Error = errImageStopped.Error()
			}

			if werr := disksource.WriteRunStatus(disksource.StatusPath(statePath), status); werr != nil {
				log(ctx).Errorf("%v", werr)
			}
		}()
	}

	err = c.imageDisks(ctx, rep, statePath, status)

	// a stopped run is not a failure, so that Task Scheduler does not retry it
	if errors.Is(err, errImageStopped) {
		status.Stopped = true

		log(ctx).Infof("The image run stopped as requested.")

		return nil
	}

	return err
}

func (c *commandImageCreate) imageDisks(ctx context.Context, rep repo.RepositoryWriter, statePath string, status *disksource.RunStatus) error {
	logf := func(format string, args ...any) { log(ctx).Infof(format, args...) }

	disks, err := disksource.DisksForVolumes(c.volumes)
	if err != nil {
		return errors.Wrap(err, "unable to find the disks to image")
	}

	plans := make([]*disksource.Plan, 0, len(disks))

	for _, d := range disks {
		plan, err := disksource.PrepareDisk(d)
		if err != nil {
			return errors.Wrapf(err, "unable to plan the image of disk %v", d)
		}

		plan.Print(c.out.stdout())
		plans = append(plans, plan)
	}

	log(ctx).Warnf("The image holds every file on these disks unencrypted, including volumes that use BitLocker, the Windows account database and saved credentials. " +
		"Only the repository's encryption protects it: keep the repository password as safe as the computer.")

	guard, err := disksource.Acquire(statePath, logf)
	if err != nil {
		return errors.Wrap(err, "unable to start imaging")
	}

	defer func() {
		if cerr := guard.Close(); cerr != nil {
			log(ctx).Errorf("unable to release the image run: %v", cerr)
		}
	}()

	// the run holds the lock, so the progress file and stop requests are its
	// own; canceling runCtx stops it, saving what was uploaded
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var monitor *imageRunMonitor

	if !c.dryRun {
		uploadedBefore := c.uploaded.Load()
		monitor = startImageRunMonitor(ctx, filepath.Dir(statePath), status.StartTime, c.volumes, len(plans), &c.progress,
			func() int64 { return c.uploaded.Load() - uploadedBefore }, cancel)

		defer monitor.close(ctx)
	}

	var drivers []image.DriverSource

	if !c.dryRun && c.drivers {
		var removeDrivers func()

		drivers, removeDrivers = c.exportDrivers(ctx, filepath.Dir(statePath))
		defer removeDrivers()
	}

	for i, plan := range plans {
		// imaged before the connection was lost
		if c.doneDisks[plan.DiskNumber] {
			continue
		}

		if monitor != nil {
			monitor.setDisk(i+1, plan.DiskNumber)
		}

		man, md, err := c.imageDisk(runCtx, rep, plan, guard.State, drivers, logf)
		if man != nil {
			status.Images = append(status.Images, runImage(man, md))
		}

		if err == nil && man != nil && c.doneDisks != nil {
			c.doneDisks[plan.DiskNumber] = true
			c.doneImages = append(c.doneImages, runImage(man, md))
		}

		if monitor != nil && monitor.stopped() {
			return errImageStopped
		}

		if err != nil {
			return errors.Wrapf(err, "unable to image disk %v", plan.DiskNumber)
		}
	}

	if !c.dryRun {
		status.RecoveryMedia = c.checkRecoveryMedia(ctx, rep, plans)
	}

	return nil
}

// exportDrivers exports this computer's storage, network and USB drivers
// into the image state folder, to store with the images, and returns them
// with a function that removes them. Drivers that cannot be exported are only
// reported: the images do not depend on them.
func (c *commandImageCreate) exportDrivers(ctx context.Context, stateDir string) ([]image.DriverSource, func()) {
	dir := filepath.Join(stateDir, "drivers-export")
	remove := func() { os.RemoveAll(dir) } //nolint:errcheck

	// left by a run that ended abruptly
	remove()

	exported, failures, err := recoverymedia.ExportHostDrivers(ctx, dir)

	switch {
	case errors.Is(err, recoverymedia.ErrNotSupported):
		return nil, remove
	case err != nil:
		log(ctx).Warnf("Unable to store this computer's drivers with the images: %v", err)
		return nil, remove
	}

	for _, f := range failures {
		log(ctx).Warnf("Driver not stored with the images: %v", f)
	}

	return exported, remove
}

// checkRecoveryMedia reports why the recovery media last built on this
// computer no longer matches it, if any was built.
func (c *commandImageCreate) checkRecoveryMedia(ctx context.Context, rep repo.Repository, plans []*disksource.Plan) []string {
	r, err := recoverymedia.ReadDefaultRecord()
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			log(ctx).Warnf("%v", err)
		}

		return nil
	}

	layouts := map[string]*image.Layout{}

	for _, p := range plans {
		path, err := image.DiskSourcePath(p.Layout.DiskGUID)
		if err != nil {
			continue
		}

		src := snapshot.SourceInfo{Host: rep.ClientOptions().Hostname, UserName: rep.ClientOptions().Username, Path: path}
		layouts[src.String()] = p.Layout
	}

	findings := recoveryMediaFindings(ctx, rep, r, c.svc.repositoryConfigFileName(), layouts)

	for _, f := range findings {
		log(ctx).Warnf("Recovery media: %v. %v.", f, recoveryMediaAdvice(r))
	}

	return findings
}

func runImage(man *snapshot.Manifest, md *image.Metadata) disksource.RunImage {
	ri := disksource.RunImage{Source: man.Source.String(), SnapshotID: string(man.ID), NewDataBytes: md.NewDataBytes, UploadedBytes: md.UploadedBytes}

	for i := range md.Volumes {
		ri.BytesRead += md.Volumes[i].Stats.BytesRead
	}

	return ri
}

// imageDisk images one disk. It returns the saved snapshot also when a later
// step, such as applying retention, fails.
func (c *commandImageCreate) imageDisk(ctx context.Context, rep repo.RepositoryWriter, plan *disksource.Plan, state *disksource.ShadowState, drivers []image.DriverSource, logf func(string, ...any)) (man *snapshot.Manifest, md *image.Metadata, err error) {
	sourcePath, err := image.DiskSourcePath(plan.Layout.DiskGUID)
	if err != nil {
		return nil, nil, errors.Wrap(err, "invalid disk GUID")
	}

	session, err := disksource.OpenSession(ctx, plan, state, disksource.SessionOptions{
		ExcludeKopiaCaches:        c.excludeCaches,
		ExcludeFilesNotToSnapshot: c.excludeFNTS,
		ExcludeFilesNotToBackup:   c.excludeFNTB,
		ExcludeCommonCaches:       c.excludeCommon,
		KopiaCacheDirs:            c.ownCacheDirs(),
	}, logf)
	if err != nil {
		return nil, nil, errors.Wrap(err, "unable to prepare the disk")
	}

	closeSession := func() error {
		return errors.Wrap(session.Close(), "unable to clean up shadow copies")
	}

	defer func() {
		err = stderrors.Join(err, closeSession())
	}()

	if c.dryRun {
		c.printDryRun(plan, session)
		return nil, nil, nil
	}

	src := snapshot.SourceInfo{Host: rep.ClientOptions().Hostname, UserName: rep.ClientOptions().Username, Path: sourcePath}

	pol, def, _, err := policy.GetEffectivePolicy(ctx, rep, src)
	if err != nil {
		return nil, nil, errors.Wrapf(err, "unable to get the policy of %v", src)
	}

	chunkSize := image.ChunkSizes[c.chunkSize]

	parallel := c.parallel
	if parallel <= 0 {
		parallel = max(runtime.GOMAXPROCS(0), image.ParallelChunks(chunkSize))
	}

	man, md, err = image.Create(ctx, rep, &image.CreateOptions{
		Source:             src,
		Description:        c.descriptionFor(plan),
		Layout:             plan.Layout,
		Volumes:            session.Volumes(),
		Compressor:         imageCompressor(pol.CompressionPolicy.CompressorName, def.CompressionPolicy.CompressorName == src),
		MetadataCompressor: pol.MetadataCompressionPolicy.MetadataCompressor(),
		AsyncWrites:        parallel,
		ChunkSize:          chunkSize,
		FlushInterval:      c.flushInterval,
		NewBuffer:          blockdev.AlignedBuffer,
		Drivers:            drivers,
		Logf:               logf,
		Progress:           &c.progress,
	})
	if err != nil {
		return nil, nil, err //nolint:wrapcheck
	}

	// Remove the shadow copies before the retention pass. The image is already
	// saved, and the next run removes shadow copies left in the state file, so
	// a failure here is only a warning.
	if serr := closeSession(); serr != nil {
		log(ctx).Warnf("%v; the next run will retry", serr)
	}

	closeSession = func() error { return nil }

	if _, err := policy.ApplyRetentionPolicy(ctx, rep, src, true); err != nil {
		return man, md, errors.Wrap(err, "unable to apply the retention policy")
	}

	if err := rep.Flush(ctx); err != nil {
		return man, md, errors.Wrap(err, "unable to flush")
	}

	c.printSummary(man, md)

	return man, md, nil
}

func (c *commandImageCreate) descriptionFor(plan *disksource.Plan) string {
	if c.description != "" {
		return c.description
	}

	var letters []string

	for i := range plan.Partitions {
		if plan.Partitions[i].Kind == image.VolumeKindNTFS && len(plan.Partitions[i].Label) == 1 {
			letters = append(letters, plan.Partitions[i].Label+":")
		}
	}

	if len(letters) == 0 {
		return "Image of disk " + plan.Layout.DiskGUID
	}

	return "Image of disk " + plan.Layout.DiskGUID + " (" + strings.Join(letters, " ") + ")"
}

// ownCacheDirs returns the cache directory of the repository connection this
// kopia uses, which may be outside the folders where caches are looked for,
// such as the scheduled task's cache in ProgramData.
func (c *commandImageCreate) ownCacheDirs() []string {
	lc, err := repo.LoadConfigFromFile(c.svc.repositoryConfigFileName())
	if err != nil || lc.Caching == nil || lc.Caching.CacheDirectory == "" {
		return nil
	}

	return []string{lc.Caching.CacheDirectory}
}

func (c *commandImageCreate) printDryRun(plan *disksource.Plan, session *disksource.Session) {
	c.out.printStdout("Dry run of disk %v; nothing was written.\n", plan.DiskNumber)

	volumes := session.Volumes()

	for i := range volumes {
		for _, line := range dryRunVolumeLines(&volumes[i]) {
			c.out.printStdout("%v\n", line)
		}
	}
}

// dryRunMaxFolders is how many of a volume's largest excluded folders a dry
// run lists.
const dryRunMaxFolders = 10

// dryRunVolumeLines describes what imaging a volume reads and leaves out: the
// excluded bytes per source and the largest excluded folders.
func dryRunVolumeLines(v *image.VolumeSource) []string {
	excluded := image.ExcludedBytes(v.Excluded)
	zeroed := v.FreeBytes + image.TotalLength(v.UnstableExtents) + excluded

	lines := []string{fmt.Sprintf("  %v: %v, %v read, %v zeroed, of which %v left out",
		image.VolumeEntryName(v.PartitionNumber, v.Label), units.BytesString(v.Size), units.BytesString(v.Size-zeroed),
		units.BytesString(zeroed), units.BytesString(excluded))}

	if len(v.Excluded) == 0 {
		return lines
	}

	type total struct {
		bytes, files int64
		folders      int
	}

	bySource := map[string]*total{}

	var sources []string

	for _, f := range v.Excluded {
		source, _, _ := strings.Cut(f.Source, `\`)

		t := bySource[source]
		if t == nil {
			t = &total{}
			bySource[source] = t
			sources = append(sources, source)
		}

		t.bytes += f.Bytes
		t.files += f.Files
		t.folders++
	}

	slices.SortFunc(sources, func(a, b string) int { return cmp.Compare(bySource[b].bytes, bySource[a].bytes) })

	for _, s := range sources {
		t := bySource[s]
		lines = append(lines, fmt.Sprintf("    %-20v %10v  %v files in %v folders", s, units.BytesString(t.bytes), groupDigits(t.files), groupDigits(t.folders)))
	}

	folders := slices.Clone(v.Excluded)
	slices.SortFunc(folders, func(a, b image.ExcludedFolder) int { return cmp.Compare(b.Bytes, a.Bytes) })

	lines = append(lines, "    largest folders:")

	for _, f := range folders[:min(len(folders), dryRunMaxFolders)] {
		path := f.Path
		if f.Pattern != "" {
			path += `\` + f.Pattern
		}

		lines = append(lines, fmt.Sprintf("    %10v  %v  (%v)", units.BytesString(f.Bytes), path, f.Source))
	}

	if more := len(folders) - dryRunMaxFolders; more > 0 {
		lines = append(lines, fmt.Sprintf("    ... and %v more folders", groupDigits(more)))
	}

	if image.RequiredFormatVersion([]image.Volume{{Excluded: v.Excluded}}) > image.FormatVersion {
		lines = append(lines, "    A restore deletes these files: it needs this version of kopia or newer, so rebuild recovery media made with older versions.")
	}

	return lines
}

func (c *commandImageCreate) printSummary(man *snapshot.Manifest, md *image.Metadata) {
	var read int64

	for i := range md.Volumes {
		read += md.Volumes[i].Stats.BytesRead
	}

	c.out.printStdout("Created image snapshot %v of %v in %v: %v read, %v new data, %v stored.\n",
		man.ID, man.Source, md.EndTime.Sub(md.StartTime).Truncate(time.Second), units.BytesString(read),
		optionalBytes(md.NewDataBytes, "?"), optionalBytes(md.UploadedBytes, "unknown (repository server)"))
}

// imageChunkSizeDefault is the name of image.DefaultChunkSize.
const imageChunkSizeDefault = "1M"

// imageChunkSizeNames returns the names of the chunk sizes, smallest first.
func imageChunkSizeNames() []string {
	names := slices.Collect(maps.Keys(image.ChunkSizes))
	slices.SortFunc(names, func(a, b string) int { return cmp.Compare(image.ChunkSizes[a], image.ChunkSizes[b]) })

	return names
}

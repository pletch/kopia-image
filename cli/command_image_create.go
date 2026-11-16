package cli

import (
	"context"
	stderrors "errors"
	"runtime"
	"strings"
	"time"

	"github.com/pkg/errors"

	"github.com/kopia/kopia/internal/units"
	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/snapshot"
	"github.com/kopia/kopia/snapshot/image"
	"github.com/kopia/kopia/snapshot/image/blockdev"
	"github.com/kopia/kopia/snapshot/image/disksource"
	"github.com/kopia/kopia/snapshot/policy"
)

const defaultImageFlushInterval = 10 * time.Minute

type commandImageCreate struct {
	volumes       []string
	parallel      int
	flushInterval time.Duration
	dryRun        bool
	description   string
	statePath     string

	out textOutput
}

func (c *commandImageCreate) setup(svc appServices, parent commandParent) {
	cmd := parent.Command("create", "Create block-level images of the disks holding the given volumes. Windows only; run as administrator or SYSTEM.")
	cmd.Arg("volume", "A volume on each disk to image, e.g. C:").Required().StringsVar(&c.volumes)
	cmd.Flag("parallel", "Number of chunks hashed and uploaded in parallel").Default("0").IntVar(&c.parallel)
	cmd.Flag("flush-interval", "How often to save upload progress, so that a failed run's uploads are reused").Default(defaultImageFlushInterval.String()).DurationVar(&c.flushInterval)
	cmd.Flag("dry-run", "Print the partition plan and the size of the zeroed space without imaging").BoolVar(&c.dryRun)
	cmd.Flag("description", "Free-form snapshot description").StringVar(&c.description)
	cmd.Flag("state-file", "Override the file that records shadow copies to remove after a crash").Hidden().StringVar(&c.statePath)
	cmd.Action(svc.repositoryWriterAction(c.run))
	c.out.setup(svc)
}

func (c *commandImageCreate) run(ctx context.Context, rep repo.RepositoryWriter) error {
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

	statePath := c.statePath
	if statePath == "" {
		if statePath, err = disksource.DefaultStatePath(); err != nil {
			return errors.Wrap(err, "unable to find the image state file")
		}
	}

	guard, err := disksource.Acquire(statePath, logf)
	if err != nil {
		return errors.Wrap(err, "unable to start imaging")
	}

	defer func() {
		if cerr := guard.Close(); cerr != nil {
			log(ctx).Errorf("unable to release the image run: %v", cerr)
		}
	}()

	for _, plan := range plans {
		if err := c.imageDisk(ctx, rep, plan, guard.State, logf); err != nil {
			return errors.Wrapf(err, "unable to image disk %v", plan.DiskNumber)
		}
	}

	return nil
}

func (c *commandImageCreate) imageDisk(ctx context.Context, rep repo.RepositoryWriter, plan *disksource.Plan, state *disksource.ShadowState, logf func(string, ...any)) (err error) {
	sourcePath, err := image.DiskSourcePath(plan.Layout.DiskGUID)
	if err != nil {
		return errors.Wrap(err, "invalid disk GUID")
	}

	src := snapshot.SourceInfo{Host: rep.ClientOptions().Hostname, UserName: rep.ClientOptions().Username, Path: sourcePath}

	pol, _, _, err := policy.GetEffectivePolicy(ctx, rep, src)
	if err != nil {
		return errors.Wrapf(err, "unable to get the policy of %v", src)
	}

	session, err := disksource.OpenSession(ctx, plan, state, logf)
	if err != nil {
		return errors.Wrap(err, "unable to prepare the disk")
	}

	closeSession := func() error {
		return errors.Wrap(session.Close(), "unable to clean up shadow copies")
	}

	defer func() {
		err = stderrors.Join(err, closeSession())
	}()

	if c.dryRun {
		c.printDryRun(plan, session)
		return nil
	}

	parallel := c.parallel
	if parallel <= 0 {
		parallel = runtime.GOMAXPROCS(0)
	}

	man, md, err := image.Create(ctx, rep, &image.CreateOptions{
		Source:             src,
		Description:        c.descriptionFor(plan),
		Layout:             plan.Layout,
		Volumes:            session.Volumes(),
		Compressor:         pol.CompressionPolicy.CompressorName,
		MetadataCompressor: pol.MetadataCompressionPolicy.MetadataCompressor(),
		AsyncWrites:        parallel,
		FlushInterval:      c.flushInterval,
		NewBuffer:          blockdev.AlignedBuffer,
		Logf:               logf,
	})
	if err != nil {
		return err //nolint:wrapcheck
	}

	// Remove the shadow copies before the retention pass. The image is already
	// saved, and the next run removes shadow copies left in the state file, so
	// a failure here is only a warning.
	if serr := closeSession(); serr != nil {
		log(ctx).Warnf("%v; the next run will retry", serr)
	}

	closeSession = func() error { return nil }

	if _, err := policy.ApplyRetentionPolicy(ctx, rep, src, true); err != nil {
		return errors.Wrap(err, "unable to apply the retention policy")
	}

	if err := rep.Flush(ctx); err != nil {
		return errors.Wrap(err, "unable to flush")
	}

	c.printSummary(man, md)

	return nil
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

func (c *commandImageCreate) printDryRun(plan *disksource.Plan, session *disksource.Session) {
	c.out.printStdout("Dry run of disk %v; nothing was written.\n", plan.DiskNumber)

	volumes := session.Volumes()

	for i := range volumes {
		v := &volumes[i]
		zeroed := v.FreeBytes + image.TotalLength(v.UnstableExtents)
		c.out.printStdout("  %v: %v, %v read, %v zeroed\n",
			image.VolumeEntryName(v.PartitionNumber, v.Label), units.BytesString(v.Size), units.BytesString(v.Size-zeroed), units.BytesString(zeroed))
	}
}

func (c *commandImageCreate) printSummary(man *snapshot.Manifest, md *image.Metadata) {
	var read int64

	for i := range md.Volumes {
		read += md.Volumes[i].Stats.BytesRead
	}

	uploaded := "unknown (repository server)"
	if md.UploadedBytes != nil {
		uploaded = units.BytesString(*md.UploadedBytes)
	}

	c.out.printStdout("Created image snapshot %v of %v in %v: %v read, %v uploaded.\n",
		man.ID, man.Source, md.EndTime.Sub(md.StartTime).Truncate(time.Second), units.BytesString(read), uploaded)
}

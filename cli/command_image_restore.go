package cli

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/pkg/errors"

	"github.com/kopia/kopia/internal/clock"
	"github.com/kopia/kopia/internal/units"
	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/manifest"
	"github.com/kopia/kopia/snapshot"
	"github.com/kopia/kopia/snapshot/image"
	"github.com/kopia/kopia/snapshot/image/imagerestore"
	"github.com/kopia/kopia/snapshot/image/recoverymedia"
)

const imageRestoreProgressEvery = 5 * time.Second

type commandImageRestore struct {
	snapshot  string
	target    string
	parallel  int
	only      []string
	dryRun    bool
	overwrite bool
	force     bool

	svc appServices
	out textOutput
}

func (c *commandImageRestore) setup(svc appServices, parent commandParent) {
	cmd := parent.Command("restore", "Restore a disk image to a disk or to a raw disk image file.")
	cmd.Arg("snapshot", "Image snapshot ID, or an image source (user@host:image:disk:<GUID>) to restore its latest image").Required().StringVar(&c.snapshot)
	cmd.Flag("target", `Disk to restore to (/dev/sdX on Linux, \\.\PhysicalDriveN on Windows) or a file; a missing file is created`).Required().StringVar(&c.target)
	cmd.Flag("parallel", "Number of chunks fetched in parallel; by default 64 MiB of chunks, 64 of 1 MiB or 16 of 4 MiB").Default("0").IntVar(&c.parallel)
	cmd.Flag("only", "Restore only these volumes (e.g. p3-C or a partition number) into the target's matching partition table").StringsVar(&c.only)
	cmd.Flag("dry-run", "Check the snapshot and the target without writing").BoolVar(&c.dryRun)
	cmd.Flag("overwrite", "Allow overwriting a target that is not blank: one with a partition table or other data").BoolVar(&c.overwrite)
	cmd.Flag("force", "Restore even if another attached disk has the image's disk GUID").BoolVar(&c.force)
	cmd.Action(svc.repositoryReaderAction(c.run))
	c.svc = svc
	c.out.setup(svc)
}

func (c *commandImageRestore) run(ctx context.Context, rep repo.Repository) error {
	man, err := resolveImageSnapshot(ctx, rep, c.snapshot)
	if err != nil {
		return err
	}

	s, err := imagerestore.LoadSnapshot(ctx, rep, man)
	if err != nil {
		return errors.Wrapf(err, "unable to load image snapshot %v", man.ID)
	}

	volumes, err := s.SelectVolumes(c.only)
	if err != nil {
		return err //nolint:wrapcheck
	}

	printImageRestorePlan(&c.out, s, volumes)

	// --only restores into the target's partition table, also when it names
	// every volume of the image
	fullRestore := len(c.only) == 0

	t, err := c.openTarget(s, volumes, fullRestore)
	if t == nil && err == nil {
		c.out.printStdout("Dry run: %v would be created as a %v-byte disk image file. Nothing was written.\n", c.target, s.Layout.DiskSize)
		return nil
	}

	if err != nil {
		return errors.Wrap(err, "unable to open the restore target")
	}

	closeTarget := sync.OnceValue(t.Close)

	defer func() {
		if cerr := closeTarget(); cerr != nil {
			log(ctx).Errorf("%v", cerr)
		}
	}()

	if fullRestore {
		if s, err = c.fitTarget(ctx, rep, s, t); err != nil {
			return err
		}

		volumes = s.Metadata.Volumes
	}

	if err := c.checkTarget(s, t, fullRestore); err != nil {
		return err
	}

	if c.dryRun {
		c.out.printStdout("Dry run: %v (%v) can hold the image. Nothing was written.\n", t.Path(), units.BytesString(t.Size()))
		return nil
	}

	if err := restoreImageToTarget(ctx, &c.out, rep, s, t, volumes, !fullRestore, c.parallel); err != nil {
		return err
	}

	// Windows mounts the restored volumes once the target is closed
	if err := closeTarget(); err != nil {
		return errors.Wrap(err, "unable to close the restore target")
	}

	if err := deleteExcludedAfterRestore(ctx, &c.out, rep, s, t, volumes); err != nil {
		return err
	}

	printResizeNotes(&c.out, s)

	return nil
}

// fitTarget returns the snapshot to restore to a target smaller than the
// imaged disk, with its NTFS volumes shrunk, or s for other targets.
func (c *commandImageRestore) fitTarget(ctx context.Context, rep repo.Repository, s *imagerestore.Snapshot, t imagerestore.Target) (*imagerestore.Snapshot, error) {
	if t.Size() >= s.Layout.MinTargetSize() {
		return s, nil
	}

	limits, err := s.LoadShrinkLimits(ctx, rep)
	if err != nil {
		return nil, errors.Wrap(err, "unable to read how far the NTFS volumes can shrink")
	}

	return fitImageToTarget(&c.out, s, limits, t.Size())
}

// openTarget opens the restore target, read-only for a dry run so that the
// target is not touched. It returns nil for a dry run to a file that does not
// exist yet.
func (c *commandImageRestore) openTarget(s *imagerestore.Snapshot, volumes []image.Volume, fullRestore bool) (imagerestore.Target, error) {
	// a partial restore writes into some partitions of the target's table
	if !fullRestore {
		var parts []image.Partition

		for i := range volumes {
			if p, ok := s.Layout.Partition(volumes[i].PartitionNumber); ok {
				parts = append(parts, p)
			}
		}

		keep := []string{systemRoot()}
		if lc, err := repo.LoadConfigFromFile(c.svc.repositoryConfigFileName()); err == nil {
			keep = append(keep, recoverymedia.LocalRepositoryPath(lc))
		}

		return imagerestore.OpenPartitionsTarget(c.target, s.Layout.SectorSize, c.dryRun, parts, keep...) //nolint:wrapcheck
	}

	if !c.dryRun {
		return imagerestore.OpenTarget(c.target, s.Layout.DiskSize, s.Layout.SectorSize) //nolint:wrapcheck
	}

	if _, err := os.Stat(c.target); errors.Is(err, os.ErrNotExist) {
		return nil, imagerestore.CheckNewTargetPath(c.target) //nolint:wrapcheck
	}

	return imagerestore.OpenTargetReadOnly(c.target, s.Layout.SectorSize) //nolint:wrapcheck
}

func (c *commandImageRestore) checkTarget(s *imagerestore.Snapshot, t imagerestore.Target, fullRestore bool) error {
	if err := s.CheckTarget(t, c.force); err != nil {
		return err //nolint:wrapcheck
	}

	if !fullRestore || c.overwrite {
		return nil
	}

	has, err := imagerestore.HasData(t)
	if err != nil {
		return err //nolint:wrapcheck
	}

	if has {
		return errors.Errorf("%v is not blank: it holds a partition table or other data; pass --overwrite to replace everything on it", t.Path())
	}

	return nil
}

// restoreImageToTarget restores the volumes of an image to an open target,
// printing progress and a summary.
func restoreImageToTarget(ctx context.Context, out *textOutput, rep repo.Repository, s *imagerestore.Snapshot, t imagerestore.Target, volumes []image.Volume, partial bool, parallel int) error {
	start := clock.Now()
	progress := newImageRestoreProgress(out.printStderr)

	if parallel <= 0 {
		parallel = image.ParallelChunks(s.Metadata.ChunkSize)
	}

	results, err := imagerestore.RestoreDisk(ctx, rep, s, t, imagerestore.DiskOptions{
		Volumes:  volumes,
		Partial:  partial,
		Parallel: parallel,
		Progress: progress.update,
	})

	progress.stop()

	// the partitions are on the disk; the operating system sees them later
	rereadErr := err
	if !errors.Is(err, imagerestore.ErrRereadPartitions) {
		rereadErr = nil
	}

	if err != nil && rereadErr == nil {
		return errors.Wrapf(err, "restore to %v failed; the target is incomplete", t.Path())
	}

	elapsed := clock.Now().Sub(start)

	var total int64

	for i := range results {
		r := &results[i]
		total += r.Stats.Bytes

		zeros := units.BytesString(r.Stats.ZeroBytes) + " of zeros"
		if r.Stats.UnusedZeroBytes > 0 {
			zeros += fmt.Sprintf(", %v of them free space that needs no writing", units.BytesString(r.Stats.UnusedZeroBytes))
		}

		out.printStdout("  %v: %v, fetched %v in %v of %v chunks, %v\n",
			r.Volume.EntryName, units.BytesString(r.Stats.Bytes), units.BytesString(r.Stats.FetchedBytes),
			r.Stats.FetchedChunks, r.Stats.Chunks, zeros)
	}

	out.printStdout("Restored %v volume(s), %v, to %v in %v (%v).\n",
		len(results), units.BytesString(total), t.Path(), elapsed.Truncate(time.Second),
		units.BytesPerSecondsString(float64(total)/max(elapsed.Seconds(), 1e-3)))
	out.printStdout("Volumes that used BitLocker are restored decrypted; re-enable BitLocker after the first boot.\n")

	if rereadErr != nil {
		out.printStderr("Warning: %v. The restore is complete; detach and attach the disk, or restart, to see its partitions.\n", rereadErr)
	}

	return nil
}

// fitImageToTarget returns the snapshot to restore to a target of size
// bytes, with its NTFS volumes shrunk and the partitions after them moved
// when the target is smaller than the imaged disk, and says what changes.
func fitImageToTarget(out *textOutput, s *imagerestore.Snapshot, limits imagerestore.ShrinkLimits, size int64) (*imagerestore.Snapshot, error) {
	fit, changes, err := s.FitTo(size, limits)
	if errors.Is(err, imagerestore.ErrTargetTooSmall) {
		return nil, errors.Errorf("the target (%v) is too small: the image needs %v, or %v with its NTFS volumes shrunk as far as their files allow",
			units.BytesString(size), units.BytesString(s.Layout.MinTargetSize()), units.BytesString(s.MinTargetSize(limits)))
	}

	if err != nil {
		return nil, err //nolint:wrapcheck
	}

	if len(changes) == 0 {
		return fit, nil
	}

	out.printStdout("The target (%v) is smaller than the imaged disk (%v). To fit:\n", units.BytesString(size), units.BytesString(s.Layout.DiskSize))

	for _, r := range changes {
		name := partitionDisplayName(s, r.Partition.Number)

		if r.Shrunk() {
			out.printStdout("  %v shrinks from %v to %v\n", name, units.BytesString(r.Partition.Length), units.BytesString(r.Length))
		}

		if r.Moved() {
			out.printStdout("  %v moves %v closer to the start of the disk\n", name, units.BytesString(r.Partition.Offset-r.Offset))
		}
	}

	return fit, nil
}

// partitionDisplayName names a partition by its volume image, if it has one.
func partitionDisplayName(s *imagerestore.Snapshot, number int) string {
	for i := range s.Metadata.Volumes {
		if v := &s.Metadata.Volumes[i]; v.PartitionNumber == number {
			return fmt.Sprintf("partition %v (%v)", number, strings.TrimSuffix(v.EntryName, ".img"))
		}
	}

	return fmt.Sprintf("partition %v", number)
}

// printResizeNotes says what to do after restoring to a smaller disk.
func printResizeNotes(out *textOutput, s *imagerestore.Snapshot) {
	for _, r := range s.Resized {
		if r.Shrunk() {
			out.printStdout("%v was shrunk: Windows checks it when it first starts, which can take a few minutes.\n", partitionDisplayName(s, r.Partition.Number))
		}
	}

	for _, r := range s.Resized {
		if r.Moved() && r.Partition.TypeGUID == image.PartitionTypeWinRecovery {
			out.printStdout("The Windows recovery partition moved. Once Windows runs, enter \"reagentc /disable\" and then \"reagentc /enable\"\n" +
				"in an administrator command prompt, so that Windows finds its recovery tools again; \"reagentc /info\" shows whether it does.\n")

			return
		}
	}
}

func printImageRestorePlan(out *textOutput, s *imagerestore.Snapshot, volumes []image.Volume) {
	out.printStdout("Image snapshot %v of %v taken %v\n", s.Manifest.ID, s.Manifest.Source, formatTimestamp(s.Manifest.StartTime.ToTime()))
	out.printStdout("Disk %v: %v, %v-byte sectors, %v partitions\n", s.Layout.DiskGUID, units.BytesString(s.Layout.DiskSize), s.Layout.SectorSize, len(s.Layout.Partitions))

	for i := range volumes {
		v := &volumes[i]
		out.printStdout("  %v -> partition %v (%v)\n", v.EntryName, v.PartitionNumber, units.BytesString(v.Size))
	}
}

// resolveImageSnapshot finds an image snapshot by manifest ID, or the latest
// complete image of a source given as user@host:image:disk:<GUID>.
func resolveImageSnapshot(ctx context.Context, rep repo.Repository, arg string) (*snapshot.Manifest, error) {
	if !strings.Contains(arg, ":"+image.SourcePathPrefix) {
		man, err := snapshot.LoadSnapshot(ctx, rep, manifest.ID(arg))
		if err != nil {
			return nil, errors.Wrapf(err, "unable to load snapshot %v", arg)
		}

		if man.Tags[image.TagKey] != image.TagValue {
			return nil, errors.Errorf("snapshot %v is not an image snapshot", arg)
		}

		return man, nil
	}

	src, err := snapshot.ParseSourceInfo(arg, rep.ClientOptions().Hostname, rep.ClientOptions().Username)
	if err != nil {
		return nil, errors.Wrapf(err, "invalid image source %v", arg)
	}

	snaps, err := snapshot.ListSnapshots(ctx, rep, src)
	if err != nil {
		return nil, errors.Wrapf(err, "unable to list the snapshots of %v", src)
	}

	var latest *snapshot.Manifest

	for _, m := range snaps {
		if m.IncompleteReason == "" && m.Tags[image.TagKey] == image.TagValue && (latest == nil || m.StartTime.After(latest.StartTime)) {
			latest = m
		}
	}

	if latest == nil {
		return nil, errors.Errorf("%v has no image snapshots", src)
	}

	return latest, nil
}

// imageRestoreProgress prints the restore progress periodically.
type imageRestoreProgress struct {
	printf func(string, ...any)

	mu sync.Mutex
	// +checklocks:mu
	entry string
	// +checklocks:mu
	done int64
	// +checklocks:mu
	total int64
	// +checklocks:mu
	start time.Time

	stopCh chan struct{}
	wg     sync.WaitGroup
}

func newImageRestoreProgress(printf func(string, ...any)) *imageRestoreProgress {
	p := &imageRestoreProgress{printf: printf, stopCh: make(chan struct{})}

	p.wg.Go(func() {
		t := time.NewTicker(imageRestoreProgressEvery)
		defer t.Stop()

		for {
			select {
			case <-p.stopCh:
				return
			case <-t.C:
				p.print()
			}
		}
	})

	return p
}

func (p *imageRestoreProgress) update(entry string, done, total int64) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if entry != p.entry {
		p.entry, p.start, p.done = entry, clock.Now(), 0
	}

	// progress callbacks from parallel workers may arrive out of order
	p.done = max(p.done, done)
	p.total = total
}

func (p *imageRestoreProgress) print() {
	p.mu.Lock()
	entry, done, total, start := p.entry, p.done, p.total, p.start
	p.mu.Unlock()

	if entry == "" || total == 0 {
		return
	}

	rate := float64(done) / max(clock.Now().Sub(start).Seconds(), 1e-3)
	p.printf("%v: %v of %v (%.0f%%), %v\n", entry, units.BytesString(done), units.BytesString(total), 100*float64(done)/float64(total), units.BytesPerSecondsString(rate))
}

func (p *imageRestoreProgress) stop() {
	close(p.stopCh)
	p.wg.Wait()
}

package cli

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/pkg/errors"

	"github.com/kopia/kopia/internal/clock"
	"github.com/kopia/kopia/internal/units"
	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/snapshot"
	"github.com/kopia/kopia/snapshot/image"
	"github.com/kopia/kopia/snapshot/image/imagerestore"
	"github.com/kopia/kopia/snapshot/image/ntfsfs"
)

type commandImageCheck struct {
	snapshots  []string
	samplePct  float64
	full       bool
	all        bool
	allHosts   bool
	parallel   int
	sampleSeed uint64
	ntfs       bool

	out textOutput
}

func (c *commandImageCheck) setup(svc appServices, parent commandParent) {
	cmd := parent.Command("check", "Verify image snapshots: every chunk is indexed and stored, and a sample of chunks reads back and authenticates.")
	cmd.Arg("snapshot", "Image snapshot IDs or image sources (user@host:image:disk:<GUID>); default: the latest image of every image source of this host").StringsVar(&c.snapshots)
	cmd.Flag("sample", "Percentage of each image's distinct chunks to fetch").Default("1").Float64Var(&c.samplePct)
	cmd.Flag("full", "Fetch every chunk").BoolVar(&c.full)
	cmd.Flag("all", "Check every image of the sources, not only the latest").BoolVar(&c.all)
	cmd.Flag("all-hosts", "With no snapshot arguments, check the images of every host").BoolVar(&c.allHosts)
	cmd.Flag("ntfs", "Also check the NTFS file system in each NTFS volume image: boot sectors, MFT records, cluster allocation and the directory tree").BoolVar(&c.ntfs)
	cmd.Flag("parallel", "Number of chunks fetched in parallel").Default("16").IntVar(&c.parallel)
	cmd.Flag("seed", "Seed for the chunk sample, to repeat a check").Hidden().Uint64Var(&c.sampleSeed)
	cmd.Action(svc.repositoryReaderAction(c.run))
	c.out.setup(svc)
}

func (c *commandImageCheck) run(ctx context.Context, rep repo.Repository) error {
	sample := c.samplePct / 100 //nolint:mnd
	if c.full {
		sample = 1
	}

	if sample < 0 || sample > 1 {
		return errors.Errorf("--sample must be between 0 and 100, got %v", c.samplePct)
	}

	mans, err := c.snapshotsToCheck(ctx, rep)
	if err != nil {
		return err
	}

	if len(mans) == 0 {
		return errors.New("no image snapshots to check")
	}

	failed := 0

	for _, man := range mans {
		ok, err := c.checkOne(ctx, rep, man, sample)
		if err != nil {
			return err
		}

		if !ok {
			failed++
		}
	}

	if failed > 0 {
		return errors.Errorf("%v of %v image snapshots failed the check", failed, len(mans))
	}

	c.out.printStdout("All %v image snapshots passed.\n", len(mans))

	return nil
}

func (c *commandImageCheck) checkOne(ctx context.Context, rep repo.Repository, man *snapshot.Manifest, sample float64) (bool, error) {
	start := clock.Now()

	c.out.printStdout("%v %v taken %v\n", man.Source, man.ID, formatTimestamp(man.StartTime.ToTime()))

	s, err := imagerestore.LoadSnapshot(ctx, rep, man)
	if err != nil {
		c.out.printStdout("  FAILED: %v\n", err)
		return false, nil
	}

	results, err := imagerestore.Check(ctx, rep, s, imagerestore.CheckOptions{
		Sample:   sample,
		Parallel: c.parallel,
		Seed:     c.sampleSeed,
	})
	if err != nil {
		return false, errors.Wrapf(err, "unable to check %v", man.ID)
	}

	ok := true

	for i := range results {
		r := &results[i]

		packs := "packs listed"
		if !r.PacksChecked {
			packs = "pack check skipped (repository server)"
		}

		status := "OK"
		if !r.OK() {
			status = "FAILED"
			ok = false
		}

		c.out.printStdout("  %v: %v, %v chunks (%v distinct), %v, fetched %v chunks (%v)\n",
			r.Volume.EntryName, status, r.Chunks, r.DistinctChunks, packs, r.FetchedChunks, units.BytesString(r.FetchedBytes))

		c.printProblems(r.Problems, r.ProblemCount)

		if c.ntfs && r.OK() && r.Volume.Kind == image.VolumeKindNTFS {
			ntfsOK, err := c.checkNTFS(ctx, rep, &r.Volume)
			if err != nil {
				return false, err
			}

			ok = ok && ntfsOK
		}
	}

	log(ctx).Debugf("checked %v in %v", man.ID, clock.Now().Sub(start).Truncate(time.Millisecond))

	return ok, nil
}

func (c *commandImageCheck) printProblems(problems []string, count int) {
	for _, p := range problems {
		c.out.printStdout("    %v\n", p)
	}

	if more := count - len(problems); more > 0 {
		c.out.printStdout("    ... and %v more problems\n", more)
	}
}

// checkNTFS checks the NTFS file system in a volume image, reading only the
// chunks holding its metadata.
func (c *commandImageCheck) checkNTFS(ctx context.Context, rep repo.Repository, v *image.Volume) (bool, error) {
	// the check reads the $MFT once, in order, so a small cache will do
	const checkCacheChunks = 8

	vol, err := ntfsfs.Open(ctx, rep, v.ObjectID, v.EntryName, ntfsfs.Options{CacheChunks: checkCacheChunks})
	if err != nil {
		c.out.printStdout("  %v file system: FAILED: %v\n", v.EntryName, err)
		return false, nil
	}
	defer vol.Close()

	r, err := vol.Check(ctx, ntfsfs.CheckOptions{})
	if err != nil {
		return false, errors.Wrapf(err, "unable to check the file system of %v", v.EntryName)
	}

	status := "OK"
	if !r.OK() {
		status = "FAILED"
	}

	c.out.printStdout("  %v file system: %v, %v files, %v directories, %v in use, read %v chunks\n",
		v.EntryName, status, r.Files, r.Directories, units.BytesString(r.ReferencedClusters*r.ClusterSize), vol.Fetches())

	c.printProblems(r.Problems, r.ProblemCount)

	return r.OK(), nil
}

// snapshotsToCheck resolves the arguments, or with none, the image sources
// of this host (all hosts with --all-hosts).
func (c *commandImageCheck) snapshotsToCheck(ctx context.Context, rep repo.Repository) ([]*snapshot.Manifest, error) {
	if len(c.snapshots) > 0 && !c.all {
		var out []*snapshot.Manifest

		for _, arg := range c.snapshots {
			man, err := resolveImageSnapshot(ctx, rep, arg)
			if err != nil {
				return nil, err
			}

			out = append(out, man)
		}

		return out, nil
	}

	ids, err := snapshot.ListSnapshotManifests(ctx, rep, nil, map[string]string{image.TagKey: image.TagValue})
	if err != nil {
		return nil, errors.Wrap(err, "unable to list image snapshots")
	}

	mans, err := snapshot.LoadSnapshots(ctx, rep, ids)
	if err != nil {
		return nil, errors.Wrap(err, "unable to load image snapshots")
	}

	keep := func(m *snapshot.Manifest) bool {
		if m.IncompleteReason != "" {
			return false
		}

		if len(c.snapshots) == 0 {
			return c.allHosts || m.Source.Host == rep.ClientOptions().Hostname
		}

		return slices.ContainsFunc(c.snapshots, func(arg string) bool {
			return arg == m.Source.String() || arg == string(m.ID)
		})
	}

	mans = slices.DeleteFunc(mans, func(m *snapshot.Manifest) bool { return !keep(m) })

	slices.SortFunc(mans, func(a, b *snapshot.Manifest) int {
		if s := strings.Compare(a.Source.String(), b.Source.String()); s != 0 {
			return s
		}

		return b.StartTime.ToTime().Compare(a.StartTime.ToTime())
	})

	if c.all {
		return mans, nil
	}

	// the latest image of each source
	return slices.CompactFunc(mans, func(a, b *snapshot.Manifest) bool { return a.Source == b.Source }), nil
}

package cli

import (
	"context"
	"strconv"
	"sync"
	"time"

	"github.com/pkg/errors"

	"github.com/kopia/kopia/internal/clock"
	"github.com/kopia/kopia/internal/units"
	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/snapshot/image/imagecompare"
	"github.com/kopia/kopia/snapshot/image/imagerestore"
)

type commandImageCompare struct {
	older, newer string

	sample   int
	seed     uint64
	parallel int
	files    bool
	depth    int
	top      int

	jo  jsonOutput
	out textOutput
}

func (c *commandImageCompare) setup(svc appServices, parent commandParent) {
	cmd := parent.Command("compare", "Measure what changed between two images of a disk: the chunks holding new data, the new data smaller chunk sizes would have stored, and the folders that caused it.")
	cmd.Arg("old", "Older image snapshot ID").Required().StringVar(&c.older)
	cmd.Arg("new", "Newer image snapshot ID, or an image source (user@host:image:disk:<GUID>) for its latest image").Required().StringVar(&c.newer)
	cmd.Flag("sample", "Number of new chunks fetched from both images and compared").Default("1000").IntVar(&c.sample)
	cmd.Flag("seed", "Seed for the chunk sample, to repeat a comparison").Uint64Var(&c.seed)
	cmd.Flag("parallel", "Number of chunk pairs fetched in parallel").Default("8").IntVar(&c.parallel)
	cmd.Flag("files", "Attribute the changed clusters of NTFS volumes to folders, which reads the MFT of both images").Default("true").BoolVar(&c.files)
	cmd.Flag("depth", "Number of path components folders are grouped by").Default("5").IntVar(&c.depth)
	cmd.Flag("top", "Number of folders listed per volume").Default("25").IntVar(&c.top)
	c.jo.setup(svc, cmd)
	cmd.Action(svc.repositoryReaderAction(c.run))
	c.out.setup(svc)
}

func (c *commandImageCompare) run(ctx context.Context, rep repo.Repository) error {
	olderMan, err := resolveImageSnapshot(ctx, rep, c.older)
	if err != nil {
		return err
	}

	newerMan, err := resolveImageSnapshot(ctx, rep, c.newer)
	if err != nil {
		return err
	}

	if olderMan.Source != newerMan.Source {
		return errors.Errorf("the images are of different disks: %v and %v", olderMan.Source, newerMan.Source)
	}

	if olderMan.ID == newerMan.ID {
		return errors.New("both arguments are the same image")
	}

	if olderMan.StartTime.After(newerMan.StartTime) {
		olderMan, newerMan = newerMan, olderMan
	}

	older, err := imagerestore.LoadSnapshot(ctx, rep, olderMan)
	if err != nil {
		return errors.Wrapf(err, "image %v", olderMan.ID)
	}

	newer, err := imagerestore.LoadSnapshot(ctx, rep, newerMan)
	if err != nil {
		return errors.Wrapf(err, "image %v", newerMan.ID)
	}

	p := &imageCompareProgress{out: &c.out}

	res, err := imagecompare.Compare(ctx, rep, older, newer, imagecompare.Options{
		Sample:    c.sample,
		Seed:      c.seed,
		Parallel:  c.parallel,
		Files:     c.files,
		PathDepth: c.depth,
		Progress:  p.report,
	})
	if err != nil {
		return errors.Wrap(err, "unable to compare the images")
	}

	if c.jo.jsonOutput {
		c.out.printStdout("%s\n", c.jo.jsonIndentedBytes(res, "  "))
		return nil
	}

	md := newer.Metadata

	c.out.printStdout("%v\n", newerMan.Source)
	c.out.printStdout("  old  %v  %v\n", formatTimestamp(olderMan.StartTime.ToTime()), olderMan.ID)
	c.out.printStdout("  new  %v  %v  (the run recorded new data %v, stored %v)\n",
		formatTimestamp(newerMan.StartTime.ToTime()), newerMan.ID, optionalBytes(md.NewDataBytes, "?"), optionalBytes(md.UploadedBytes, "?"))
	c.out.printStdout("  sample seed %v: repeat with --seed=%v\n", res.Seed, res.Seed)

	for _, u := range res.Unmatched {
		c.out.printStdout("  not compared: %v\n", u)
	}

	for i := range res.Volumes {
		c.printVolume(&res.Volumes[i])
	}

	return nil
}

func (c *commandImageCompare) printVolume(v *imagecompare.VolumeResult) {
	c.out.printStdout("\n%v (%v, %v chunks of %v)\n", v.EntryName, v.Kind, groupDigits(v.Chunks), units.BytesStringBase2(v.ChunkSize))
	c.out.printStdout("  unchanged %v, moved %v, new %v (%v)\n",
		groupDigits(v.UnchangedChunks), groupDigits(v.MovedChunks), groupDigits(v.NewChunks), units.BytesString(v.NewChunkBytes))

	if v.NewChunks == 0 {
		return
	}

	c.out.printStdout("  compared %v new chunks (%v): about %v changed in 4 KiB units\n",
		groupDigits(v.SampledChunks), units.BytesString(v.SampledBytes), units.BytesString(v.ChangedBytes))

	var current int64

	for _, b := range v.Blocks {
		if b.Size == v.ChunkSize {
			current = b.NewBytes
		}
	}

	c.out.printStdout("\n  %-12v %16v %8v %20v\n", "chunk size", "new data (est.)", "vs now", "chunk-list entries")

	for _, b := range v.Blocks {
		rel := "-"
		if current > 0 {
			rel = strconv.FormatInt(b.NewBytes*100/current, 10) + "%"
		}

		c.out.printStdout("  %-12v %16v %8v %20v\n", units.BytesStringBase2(b.Size), units.BytesString(b.NewBytes), rel, groupDigits(b.ListEntries))
	}

	if v.FilesError != "" {
		c.out.printStdout("\n  folders not attributed: %v\n", v.FilesError)
	}

	if len(v.Groups) == 0 {
		return
	}

	shown := v.Groups
	if c.top > 0 && len(shown) > c.top {
		shown = shown[:c.top]
	}

	c.out.printStdout("\n  folders by the new chunk data they caused (est.), %v of %v:\n", len(shown), len(v.Groups))
	c.out.printStdout("  %12v %12v  %v\n", "chunk data", "changed", "folder")

	for _, g := range shown {
		c.out.printStdout("  %12v %12v  %v\n", units.BytesString(g.ChunkCost), units.BytesString(g.ChangedBytes), g.Path)
	}
}

// imageCompareProgress prints the progress of a comparison to stderr, at
// most twice a second.
type imageCompareProgress struct {
	out *textOutput

	mu   sync.Mutex
	last time.Time
}

const imageCompareProgressInterval = 500 * time.Millisecond

func (p *imageCompareProgress) report(volume, stage string, done, total int64) {
	p.mu.Lock()
	defer p.mu.Unlock()

	finished := done >= total

	if now := clock.Now(); finished || now.Sub(p.last) >= imageCompareProgressInterval {
		p.last = now

		pct := int64(100) //nolint:mnd
		if total > 0 {
			pct = done * 100 / total //nolint:mnd
		}

		p.out.printStderr("\r  %v: %v %v%%   ", volume, stage, pct)

		if finished {
			p.out.printStderr("\n")
		}
	}
}

// groupDigits formats a count exactly, with thousands separated by commas.
func groupDigits[T int | int64](n T) string {
	s := strconv.FormatInt(int64(n), 10)

	start := 0
	if n < 0 {
		start = 1
	}

	for i := len(s) - 3; i > start; i -= 3 { //nolint:mnd
		s = s[:i] + "," + s[i:]
	}

	return s
}

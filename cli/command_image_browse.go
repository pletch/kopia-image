package cli

import (
	"context"
	"math"
	"path/filepath"
	"slices"
	"strings"

	"github.com/pkg/errors"

	"github.com/kopia/kopia/fs"
	"github.com/kopia/kopia/internal/units"
	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/snapshot/image"
	"github.com/kopia/kopia/snapshot/image/imagerestore"
	"github.com/kopia/kopia/snapshot/image/ntfsfs"
	"github.com/kopia/kopia/snapshot/restore"
)

const imagePathHelp = "Image snapshot ID or image source, then the volume and a path in it, e.g. <id>/p3-C/Users/tim"

// imageBrowsePath is "<snapshot|source>/<volume>/<path>" resolved to an
// image snapshot and, when given, an open volume and an entry in it.
type imageBrowsePath struct {
	snapshot *imagerestore.Snapshot
	volume   *ntfsfs.Volume
	entry    fs.Entry
}

func (p *imageBrowsePath) close() {
	if p.volume != nil {
		p.volume.Close()
	}
}

// openImagePath resolves a browse path. Without a volume it returns only the
// snapshot. Paths inside the volume may use / or \.
func openImagePath(ctx context.Context, rep repo.Repository, arg string, opt ntfsfs.Options) (*imageBrowsePath, error) {
	head, rest, _ := strings.Cut(arg, "/")

	man, err := resolveImageSnapshot(ctx, rep, head)
	if err != nil {
		return nil, err
	}

	s, err := imagerestore.LoadSnapshot(ctx, rep, man)
	if err != nil {
		return nil, errors.Wrapf(err, "unable to load image snapshot %v", man.ID)
	}

	p := &imageBrowsePath{snapshot: s}

	volName, inner, _ := strings.Cut(strings.ReplaceAll(rest, `\`, "/"), "/")
	if volName == "" {
		return p, nil
	}

	vols, err := s.SelectVolumes([]string{volName})
	if err != nil {
		return nil, err //nolint:wrapcheck
	}

	v := vols[0]
	if v.Kind != image.VolumeKindNTFS {
		return nil, errors.Errorf("%v is a %v image; only NTFS volumes can be browsed (restore it with 'kopia image restore --only %v')", v.EntryName, v.Kind, v.PartitionNumber)
	}

	// files read as zeros are left out, as a restore deletes them
	opt.Excluded, err = image.ReadExcludedFiles(ctx, rep, &v)
	if err != nil {
		return nil, err //nolint:wrapcheck
	}

	p.volume, err = ntfsfs.Open(ctx, rep, v.ObjectID, strings.TrimSuffix(v.EntryName, ".img"), opt)
	if err != nil {
		return nil, errors.Wrapf(err, "unable to open %v", v.EntryName)
	}

	p.entry = p.volume.Root()

	for part := range strings.SplitSeq(inner, "/") {
		if part == "" {
			continue
		}

		d, ok := p.entry.(fs.Directory)
		if !ok {
			p.close()
			return nil, errors.Errorf("%v is not a folder", p.entry.Name())
		}

		if p.entry, err = d.Child(ctx, part); err != nil {
			p.close()
			return nil, errors.Wrapf(err, "%v not found", part)
		}
	}

	return p, nil
}

type commandImageLs struct {
	path string
	long bool

	out textOutput
}

func (c *commandImageLs) setup(svc appServices, parent commandParent) {
	cmd := parent.Command("ls", "List the volumes of an image snapshot, or the files in an NTFS volume image.")
	cmd.Arg("path", imagePathHelp).Required().StringVar(&c.path)
	cmd.Flag("long", "Show sizes and modification times").Short('l').BoolVar(&c.long)
	cmd.Action(svc.repositoryReaderAction(c.run))
	c.out.setup(svc)
}

func (c *commandImageLs) run(ctx context.Context, rep repo.Repository) error {
	p, err := openImagePath(ctx, rep, c.path, ntfsfs.Options{})
	if err != nil {
		return err
	}
	defer p.close()

	if p.volume == nil {
		for i := range p.snapshot.Metadata.Volumes {
			v := &p.snapshot.Metadata.Volumes[i]
			c.out.printStdout("%-20v %-5v %12v\n", strings.TrimSuffix(v.EntryName, ".img"), v.Kind, units.BytesString(v.Size))
		}

		return nil
	}

	d, ok := p.entry.(fs.Directory)
	if !ok {
		c.printEntry(p.entry)
		return nil
	}

	entries, err := fs.GetAllEntries(ctx, d)
	if err != nil {
		return errors.Wrap(err, "unable to list the folder")
	}

	slices.SortFunc(entries, func(a, b fs.Entry) int { return strings.Compare(strings.ToLower(a.Name()), strings.ToLower(b.Name())) })

	for _, e := range entries {
		c.printEntry(e)
	}

	return nil
}

func (c *commandImageLs) printEntry(e fs.Entry) {
	name := e.Name()
	if e.IsDir() {
		name += "/"
	}

	if !c.long {
		c.out.printStdout("%v\n", name)
		return
	}

	size := ""
	if !e.IsDir() {
		size = units.BytesString(e.Size())
	}

	c.out.printStdout("%v %12v  %v\n", formatTimestamp(e.ModTime()), size, name)
}

type commandImageRestoreFiles struct {
	path           string
	target         string
	overwriteFiles bool
	parallel       int

	out textOutput
}

func (c *commandImageRestoreFiles) setup(svc appServices, parent commandParent) {
	cmd := parent.Command("restore-files", "Restore files or folders from an NTFS volume image without restoring the disk.")
	cmd.Arg("path", imagePathHelp).Required().StringVar(&c.path)
	cmd.Arg("target", "Folder to restore into").Required().StringVar(&c.target)
	cmd.Flag("overwrite-files", "Overwrite existing files").BoolVar(&c.overwriteFiles)
	cmd.Flag("parallel", "Number of files restored in parallel").Default("8").IntVar(&c.parallel)
	cmd.Action(svc.repositoryReaderAction(c.run))
	c.out.setup(svc)
}

func (c *commandImageRestoreFiles) run(ctx context.Context, rep repo.Repository) error {
	// streams written in parallel with their files could be lost; a full
	// image restore preserves them
	p, err := openImagePath(ctx, rep, c.path, ntfsfs.Options{SkipStreams: true})
	if err != nil {
		return err
	}
	defer p.close()

	if p.volume == nil {
		return errors.New("specify a volume and a path in it, e.g. <id>/p3-C/Users/tim")
	}

	targetPath := filepath.Join(c.target, p.entry.Name())

	output := &restore.FilesystemOutput{
		TargetPath:           targetPath,
		OverwriteDirectories: true,
		OverwriteFiles:       c.overwriteFiles,
		SkipOwners:           true,
		SkipPermissions:      true,
	}

	if err := output.Init(ctx); err != nil {
		return errors.Wrap(err, "unable to prepare the target")
	}

	st, err := restore.Entry(ctx, rep, output, p.entry, restore.Options{
		Parallel: c.parallel,
		// restore everything; shallow placeholders need snapshot directory entries
		RestoreDirEntryAtDepth: math.MaxInt32,
	})
	if err != nil {
		if !c.overwriteFiles && strings.Contains(err.Error(), "already exists") {
			return errors.Wrap(err, "restore failed; pass --overwrite-files to replace existing files")
		}

		return errors.Wrap(err, "restore failed")
	}

	c.out.printStdout("Restored %v files (%v) and %v folders to %v; read %v image chunks.\n",
		st.RestoredFileCount, units.BytesString(st.RestoredTotalFileSize), st.RestoredDirCount, targetPath, p.volume.Fetches())

	return nil
}

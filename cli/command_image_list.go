package cli

import (
	"context"
	"encoding/json"
	"io"
	"slices"
	"strings"

	"github.com/pkg/errors"

	"github.com/kopia/kopia/fs"
	"github.com/kopia/kopia/internal/units"
	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/snapshot"
	"github.com/kopia/kopia/snapshot/image"
	"github.com/kopia/kopia/snapshot/snapshotfs"
)

const maxImageMetadataSize = 16 << 20

type commandImageList struct {
	all bool

	out textOutput
}

func (c *commandImageList) setup(svc appServices, parent commandParent) {
	cmd := parent.Command("list", "List image snapshots with their statistics.").Alias("ls")
	cmd.Flag("all", "List images of all hosts, not only this one").BoolVar(&c.all)
	cmd.Action(svc.repositoryReaderAction(c.run))
	c.out.setup(svc)
}

func (c *commandImageList) run(ctx context.Context, rep repo.Repository) error {
	ids, err := snapshot.ListSnapshotManifests(ctx, rep, nil, map[string]string{image.TagKey: image.TagValue})
	if err != nil {
		return errors.Wrap(err, "unable to list image snapshots")
	}

	mans, err := snapshot.LoadSnapshots(ctx, rep, ids)
	if err != nil {
		return errors.Wrap(err, "unable to load image snapshots")
	}

	host := rep.ClientOptions().Hostname
	mans = slices.DeleteFunc(mans, func(m *snapshot.Manifest) bool { return !c.all && m.Source.Host != host })

	slices.SortFunc(mans, func(a, b *snapshot.Manifest) int {
		if s := strings.Compare(a.Source.String(), b.Source.String()); s != 0 {
			return s
		}

		return a.StartTime.ToTime().Compare(b.StartTime.ToTime())
	})

	if len(mans) == 0 {
		c.out.printStderr("No image snapshots found.\n")
		return nil
	}

	var lastSource snapshot.SourceInfo

	for _, m := range mans {
		if m.Source != lastSource {
			c.out.printStdout("%v\n", m.Source)
			lastSource = m.Source
		}

		md, err := readImageMetadata(ctx, rep, m)
		if err != nil {
			c.out.printStdout("  %v %v  unable to read %v: %v\n", formatTimestamp(m.StartTime.ToTime()), m.ID, image.MetadataFileName, err)
			continue
		}

		c.out.printStdout("  %v\n", formatImageListLine(m, md))
	}

	return nil
}

func formatImageListLine(m *snapshot.Manifest, md *image.Metadata) string {
	var read, chunks, zero int64

	for i := range md.Volumes {
		read += md.Volumes[i].Stats.BytesRead
		chunks += md.Volumes[i].Stats.ChunksTotal
		zero += md.Volumes[i].Stats.ZeroChunks
	}

	var size int64
	if m.RootEntry != nil && m.RootEntry.DirSummary != nil {
		size = m.RootEntry.DirSummary.TotalFileSize
	}

	uploaded := "?"
	if md.UploadedBytes != nil {
		uploaded = units.BytesString(*md.UploadedBytes)
	}

	return formatTimestamp(m.StartTime.ToTime()) + " " + string(m.ID) +
		"  size " + units.BytesString(size) +
		"  read " + units.BytesString(read) +
		"  uploaded " + uploaded +
		"  zero chunks " + units.Count(zero) + "/" + units.Count(chunks) +
		"  took " + md.EndTime.Sub(md.StartTime).Round(1e9).String()
}

func readImageMetadata(ctx context.Context, rep repo.Repository, m *snapshot.Manifest) (*image.Metadata, error) {
	root, err := snapshotfs.SnapshotRoot(rep, m)
	if err != nil {
		return nil, errors.Wrap(err, "unable to open snapshot root")
	}

	dir, ok := root.(fs.Directory)
	if !ok {
		return nil, errors.New("snapshot root is not a directory")
	}

	e, err := dir.Child(ctx, image.MetadataFileName)
	if err != nil {
		return nil, errors.Wrap(err, "not found")
	}

	f, ok := e.(fs.File)
	if !ok {
		return nil, errors.New("not a file")
	}

	r, err := f.Open(ctx)
	if err != nil {
		return nil, errors.Wrap(err, "unable to open")
	}
	defer r.Close() //nolint:errcheck

	b, err := io.ReadAll(io.LimitReader(r, maxImageMetadataSize))
	if err != nil {
		return nil, errors.Wrap(err, "unable to read")
	}

	var md image.Metadata

	if err := json.Unmarshal(b, &md); err != nil {
		return nil, errors.Wrap(err, "invalid JSON")
	}

	return &md, nil
}

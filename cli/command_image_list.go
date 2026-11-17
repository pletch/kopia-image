package cli

import (
	"context"
	"slices"
	"strings"

	"github.com/kopia/kopia/internal/units"
	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/snapshot"
	"github.com/kopia/kopia/snapshot/image"
)

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
	mans, err := image.ListSnapshots(ctx, rep)
	if err != nil {
		return err //nolint:wrapcheck
	}

	host := rep.ClientOptions().Hostname
	mans = slices.DeleteFunc(mans, func(m *snapshot.Manifest) bool {
		return !c.all && m.Source.Host != host
	})

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

		md, err := image.ReadMetadata(ctx, rep, m)
		if err != nil {
			c.out.printStdout("  %v %v  unable to read %v: %v\n", formatTimestamp(m.StartTime.ToTime()), m.ID, image.MetadataFileName, err)
			continue
		}

		c.out.printStdout("  %v\n", formatImageListLine(m, md))
	}

	return nil
}

func formatImageListLine(m *snapshot.Manifest, md *image.Metadata) string {
	t := md.Totals()

	var size int64
	if m.RootEntry != nil && m.RootEntry.DirSummary != nil {
		size = m.RootEntry.DirSummary.TotalFileSize
	}

	return formatTimestamp(m.StartTime.ToTime()) + " " + string(m.ID) +
		"  size " + units.BytesString(size) +
		"  read " + units.BytesString(t.BytesRead) +
		"  new data " + optionalBytes(md.NewDataBytes, "?") +
		"  stored " + optionalBytes(md.UploadedBytes, "?") +
		"  zero chunks " + units.Count(t.ZeroChunks) + "/" + units.Count(t.ChunksTotal) +
		"  took " + md.EndTime.Sub(md.StartTime).Round(1e9).String()
}

// optionalBytes formats a byte count that may not be known.
func optionalBytes(n *int64, unknown string) string {
	if n == nil {
		return unknown
	}

	return units.BytesString(*n)
}

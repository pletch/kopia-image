package image

import (
	"context"
	"encoding/json"
	"io"

	"github.com/pkg/errors"

	"github.com/kopia/kopia/fs"
	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/snapshot"
	"github.com/kopia/kopia/snapshot/snapshotfs"
)

// maxMetadataSize bounds the image.json read from a snapshot.
const maxMetadataSize = 16 << 20

// ListSnapshots returns the complete image snapshots of every host, in no
// particular order. Checkpoints of runs in progress or failed are left out:
// they are not images.
func ListSnapshots(ctx context.Context, rep repo.Repository) ([]*snapshot.Manifest, error) {
	ids, err := snapshot.ListSnapshotManifests(ctx, rep, nil, map[string]string{TagKey: TagValue})
	if err != nil {
		return nil, errors.Wrap(err, "unable to list image snapshots")
	}

	mans, err := snapshot.LoadSnapshots(ctx, rep, ids)
	if err != nil {
		return nil, errors.Wrap(err, "unable to load image snapshots")
	}

	complete := mans[:0]

	for _, m := range mans {
		if m.IncompleteReason == "" {
			complete = append(complete, m)
		}
	}

	return complete, nil
}

// ReadMetadata reads the image.json of an image snapshot.
func ReadMetadata(ctx context.Context, rep repo.Repository, m *snapshot.Manifest) (*Metadata, error) {
	root, err := snapshotfs.SnapshotRoot(rep, m)
	if err != nil {
		return nil, errors.Wrap(err, "unable to open snapshot root")
	}

	dir, ok := root.(fs.Directory)
	if !ok {
		return nil, errors.New("snapshot root is not a directory")
	}

	e, err := dir.Child(ctx, MetadataFileName)
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

	b, err := io.ReadAll(io.LimitReader(r, maxMetadataSize))
	if err != nil {
		return nil, errors.Wrap(err, "unable to read")
	}

	var md Metadata

	if err := json.Unmarshal(b, &md); err != nil {
		return nil, errors.Wrap(err, "invalid JSON")
	}

	return &md, nil
}

// FillBytesRead sets ImageBytesRead of image snapshots taken before it was
// recorded in the manifest, from their image.json. Other snapshots, and
// images whose metadata cannot be read, are left unchanged.
func FillBytesRead(ctx context.Context, rep repo.Repository, mans []*snapshot.Manifest) {
	for _, m := range mans {
		if m == nil || m.ImageBytesRead != nil || m.Tags[TagKey] != TagValue {
			continue
		}

		md, err := ReadMetadata(ctx, rep, m)
		if err != nil {
			continue
		}

		n := md.Totals().BytesRead
		m.ImageBytesRead = &n
	}
}

// Totals are the statistics of an image summed over its volumes.
type Totals struct {
	BytesRead   int64 `json:"bytesRead"`
	ChunksTotal int64 `json:"chunksTotal"`
	ZeroChunks  int64 `json:"zeroChunks"`
}

// Totals sums the statistics of the image's volumes.
func (m *Metadata) Totals() Totals {
	var t Totals

	for i := range m.Volumes {
		t.BytesRead += m.Volumes[i].Stats.BytesRead
		t.ChunksTotal += m.Volumes[i].Stats.ChunksTotal
		t.ZeroChunks += m.Volumes[i].Stats.ZeroChunks
	}

	return t
}

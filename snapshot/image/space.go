package image

import (
	"context"
	"sync"

	"github.com/pkg/errors"

	"github.com/kopia/kopia/fs"
	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/content"
	"github.com/kopia/kopia/repo/manifest"
	"github.com/kopia/kopia/repo/object"
	"github.com/kopia/kopia/snapshot"
	"github.com/kopia/kopia/snapshot/snapshotfs"
)

// Space is the repository space that a set of images, usually those of one
// disk, takes. Images share the chunks that did not change between them, so
// the space of each image's run (UploadedBytes) does not add up to it.
type Space struct {
	// StoredBytes is the size, as stored, of the contents the images use,
	// each counted once.
	StoredBytes int64

	// ExclusiveBytes is, by image, the stored size of the contents that no
	// other image of the set uses: about what deleting the image frees, once
	// maintenance removes the contents nothing uses.
	ExclusiveBytes map[manifest.ID]int64
}

// CalculateSpace reads which contents each image uses, from the index of the
// repository and the chunk lists of the images; it reads no image data.
func CalculateSpace(ctx context.Context, rep repo.Repository, mans []*snapshot.Manifest) (*Space, error) {
	refs := map[content.ID]int32{}
	perImage := make([][]content.ID, len(mans))

	for i, m := range mans {
		ids, err := imageContents(ctx, rep, m)
		if err != nil {
			return nil, errors.Wrapf(err, "unable to read the contents of image %v", m.ID)
		}

		perImage[i] = ids

		for _, id := range ids {
			refs[id]++
		}
	}

	stored := make(map[content.ID]int64, len(refs))
	s := &Space{ExclusiveBytes: map[manifest.ID]int64{}}

	for id := range refs {
		if err := ctx.Err(); err != nil {
			return nil, err //nolint:wrapcheck
		}

		info, err := rep.ContentInfo(ctx, id)
		if err != nil {
			return nil, errors.Wrapf(err, "unable to read the size of content %v", id)
		}

		stored[id] = int64(info.PackedLength)
		s.StoredBytes += int64(info.PackedLength)
	}

	for i, m := range mans {
		var exclusive int64

		for _, id := range perImage[i] {
			if refs[id] == 1 {
				exclusive += stored[id]
			}
		}

		s.ExclusiveBytes[m.ID] = exclusive
	}

	return s, nil
}

// imageContents returns the contents that an image uses, each once: those
// of its volumes, chunk lists, files and folders.
func imageContents(ctx context.Context, rep repo.Repository, m *snapshot.Manifest) ([]content.ID, error) {
	var (
		mu  sync.Mutex
		ids = map[content.ID]struct{}{}
	)

	tw, err := snapshotfs.NewTreeWalker(ctx, snapshotfs.TreeWalkerOptions{
		EntryCallback: func(ctx context.Context, _ fs.Entry, oid object.ID, _ string) error {
			cids, err := rep.VerifyObject(ctx, oid)
			if err != nil {
				return errors.Wrapf(err, "unable to read object %v", oid)
			}

			mu.Lock()
			defer mu.Unlock()

			for _, cid := range cids {
				ids[cid] = struct{}{}
			}

			return nil
		},
	})
	if err != nil {
		return nil, errors.Wrap(err, "tree walker")
	}
	defer tw.Close(ctx)

	root, err := snapshotfs.SnapshotRoot(rep, m)
	if err != nil {
		return nil, errors.Wrap(err, "unable to open the image")
	}

	if err := tw.Process(ctx, root, string(m.ID)); err != nil {
		return nil, errors.Wrap(err, "unable to walk the image")
	}

	out := make([]content.ID, 0, len(ids))
	for id := range ids {
		out = append(out, id)
	}

	return out, nil
}

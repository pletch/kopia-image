package image

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/pkg/errors"
	"golang.org/x/sync/errgroup"

	"github.com/kopia/kopia/fs"
	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/content"
	"github.com/kopia/kopia/repo/manifest"
	"github.com/kopia/kopia/repo/object"
	"github.com/kopia/kopia/snapshot"
	"github.com/kopia/kopia/snapshot/snapshotfs"
)

// spaceLookups is how many content sizes are looked up at once: through a
// repository server each is a round trip.
const spaceLookups = 32

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

// CalculateSpace reads which contents each image uses, from the chunk lists
// of the images, and their stored sizes from the index of the repository; it
// reads no image data.
func CalculateSpace(ctx context.Context, rep repo.Repository, mans []*snapshot.Manifest) (*Space, error) {
	contents := map[manifest.ID][]content.ID{}

	var all []content.ID

	for _, m := range mans {
		ids, err := Contents(ctx, rep, m)
		if err != nil {
			return nil, err
		}

		contents[m.ID] = ids
		all = append(all, ids...)
	}

	sizes := map[content.ID]int64{}
	if err := StoredSizes(ctx, rep, all, sizes); err != nil {
		return nil, err
	}

	return SpaceOf(mans, contents, sizes), nil
}

// SpaceOf adds up the space of images from the contents each uses and the
// stored size of each content.
func SpaceOf(mans []*snapshot.Manifest, contents map[manifest.ID][]content.ID, sizes map[content.ID]int64) *Space {
	refs := map[content.ID]int32{}

	for _, m := range mans {
		for _, id := range contents[m.ID] {
			refs[id]++
		}
	}

	s := &Space{ExclusiveBytes: map[manifest.ID]int64{}}

	for id := range refs {
		s.StoredBytes += sizes[id]
	}

	for _, m := range mans {
		var exclusive int64

		for _, id := range contents[m.ID] {
			if refs[id] == 1 {
				exclusive += sizes[id]
			}
		}

		s.ExclusiveBytes[m.ID] = exclusive
	}

	return s
}

// StoredSizes adds to sizes the stored size of each of ids it lacks, looking
// several up at once.
func StoredSizes(ctx context.Context, rep repo.Repository, ids []content.ID, sizes map[content.ID]int64) error {
	var missing []content.ID

	seen := map[content.ID]bool{}

	for _, id := range ids {
		if _, ok := sizes[id]; !ok && !seen[id] {
			seen[id] = true
			missing = append(missing, id)
		}
	}

	var mu sync.Mutex

	eg, ctx := errgroup.WithContext(ctx)
	eg.SetLimit(spaceLookups)

	for _, id := range missing {
		eg.Go(func() error {
			info, err := rep.ContentInfo(ctx, id)
			if err != nil {
				return errors.Wrapf(err, "unable to read the size of content %v", id)
			}

			mu.Lock()
			sizes[id] = int64(info.PackedLength)
			mu.Unlock()

			return nil
		})
	}

	return eg.Wait() //nolint:wrapcheck
}

// Contents returns the contents that an image uses, each once: those
// of its volumes and their chunk lists, files and folders. It reads chunk
// lists, not the index entry of every chunk, so that through a repository
// server it takes a few requests rather than one per chunk.
func Contents(ctx context.Context, rep repo.Repository, m *snapshot.Manifest) ([]content.ID, error) {
	var (
		mu  sync.Mutex
		ids = map[content.ID]struct{}{}
	)

	tw, err := snapshotfs.NewTreeWalker(ctx, snapshotfs.TreeWalkerOptions{
		EntryCallback: func(ctx context.Context, _ fs.Entry, oid object.ID, _ string) error {
			cids, err := objectContents(ctx, rep, oid)
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
		return nil, errors.Wrapf(err, "unable to open image %v", m.ID)
	}

	if err := tw.Process(ctx, root, string(m.ID)); err != nil {
		return nil, errors.Wrapf(err, "unable to read the contents of image %v", m.ID)
	}

	out := make([]content.ID, 0, len(ids))
	for id := range ids {
		out = append(out, id)
	}

	return out, nil
}

// objectContents returns the contents of an object: its own content, or for
// a multi-chunk object the contents of its chunk list and of its chunks.
func objectContents(ctx context.Context, rep repo.Repository, oid object.ID) ([]content.ID, error) {
	index, ok := oid.IndexObjectID()
	if !ok {
		cid, _, ok := oid.ContentID()
		if !ok {
			return nil, errors.Errorf("unsupported object ID %v", oid)
		}

		return []content.ID{cid}, nil
	}

	out, err := objectContents(ctx, rep, index)
	if err != nil {
		return nil, err
	}

	r, err := rep.OpenObject(ctx, index)
	if err != nil {
		return nil, errors.Wrap(err, "unable to open the chunk list")
	}
	defer r.Close() //nolint:errcheck

	var list struct {
		Entries []object.IndirectObjectEntry `json:"entries"`
	}

	if err := json.NewDecoder(r).Decode(&list); err != nil {
		return nil, errors.Wrap(err, "invalid chunk list")
	}

	for _, e := range list.Entries {
		cids, err := objectContents(ctx, rep, e.Object)
		if err != nil {
			return nil, err
		}

		out = append(out, cids...)
	}

	return out, nil
}

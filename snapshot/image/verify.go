package image

import (
	"context"
	"sync"

	"github.com/pkg/errors"
	"golang.org/x/sync/errgroup"

	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/content"
	"github.com/kopia/kopia/repo/object"
)

// VerifyObject checks that the repository holds every content of an image
// object, its chunk list and its chunks, and returns their index entries. It
// asks about parallel contents at once: through a repository server each
// question is a round trip, and a volume of 1 MiB chunks has a million of
// them per terabyte, which one at a time, as repo.VerifyObject asks, took
// many minutes. When progress is set, it is told how many of the distinct
// contents were checked, out of how many.
func VerifyObject(ctx context.Context, rep repo.Repository, oid object.ID, parallel int, progress func(done, total int64)) (map[content.ID]content.Info, error) {
	ids, err := objectContents(ctx, rep, oid)
	if err != nil {
		return nil, err
	}

	var distinct []content.ID

	seen := make(map[content.ID]bool, len(ids))

	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			distinct = append(distinct, id)
		}
	}

	if progress == nil {
		progress = func(int64, int64) {}
	}

	total := int64(len(distinct))
	progress(0, total)

	var (
		mu      sync.Mutex
		checked int64
	)

	infos := make(map[content.ID]content.Info, len(distinct))

	eg, ctx := errgroup.WithContext(ctx)
	eg.SetLimit(max(1, parallel))

	for _, id := range distinct {
		eg.Go(func() error {
			info, err := rep.ContentInfo(ctx, id)
			if err != nil {
				return errors.Wrapf(err, "error getting content info for %v", id)
			}

			// a content marked deleted is still readable today, but maintenance
			// drops it once it is old enough, so the image would lose it.
			if info.Deleted {
				return errors.Errorf("content %v is marked deleted", id)
			}

			mu.Lock()
			infos[id] = info
			checked++
			done := checked
			mu.Unlock()

			progress(done, total)

			return nil
		})
	}

	if err := eg.Wait(); err != nil {
		return nil, err //nolint:wrapcheck
	}

	return infos, nil
}

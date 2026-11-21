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
// many minutes.
func VerifyObject(ctx context.Context, rep repo.Repository, oid object.ID, parallel int) (map[content.ID]content.Info, error) {
	ids, err := objectContents(ctx, rep, oid)
	if err != nil {
		return nil, err
	}

	var mu sync.Mutex

	infos := make(map[content.ID]content.Info, len(ids))
	seen := make(map[content.ID]bool, len(ids))

	eg, ctx := errgroup.WithContext(ctx)
	eg.SetLimit(max(1, parallel))

	for _, id := range ids {
		if seen[id] {
			continue
		}

		seen[id] = true

		eg.Go(func() error {
			info, err := rep.ContentInfo(ctx, id)
			if err != nil {
				return errors.Wrapf(err, "error getting content info for %v", id)
			}

			mu.Lock()
			infos[id] = info
			mu.Unlock()

			return nil
		})
	}

	if err := eg.Wait(); err != nil {
		return nil, err //nolint:wrapcheck
	}

	return infos, nil
}

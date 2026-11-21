package server

import (
	"context"
	"sync"

	"github.com/pkg/errors"

	"github.com/kopia/kopia/fs"
	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/manifest"
	"github.com/kopia/kopia/snapshot"
	"github.com/kopia/kopia/snapshot/image"
)

// imageDetailsCache keeps the layout and metadata of each image. An image's
// files never change once it is written, so listing the images reads them
// from the repository once, not on every request: through a repository
// server each read is a round trip.
type imageDetailsCache struct {
	mu sync.Mutex

	// +checklocks:mu
	entries map[manifest.ID]*imageDetails
}

// imageDetails is what was read of an image. A file that could not be read
// is not kept, so it is read again next time, unless the image has none.
type imageDetails struct {
	layoutRead bool
	layout     *image.Layout // nil for an image without one

	metadata *image.Metadata
}

// get returns the layout of an image (nil if it has none or it cannot be
// read) and its metadata, reading from the repository what is not kept.
func (c *imageDetailsCache) get(ctx context.Context, rep repo.Repository, m *snapshot.Manifest) (*image.Layout, *image.Metadata, error) {
	var d imageDetails

	c.mu.Lock()
	if e := c.entries[m.ID]; e != nil {
		d = *e
	}
	c.mu.Unlock()

	if d.layoutRead && d.metadata != nil {
		return d.layout, d.metadata, nil
	}

	// the reads happen outside the lock; two requests may both read an image
	if !d.layoutRead {
		l, err := image.ReadLayout(ctx, rep, m)

		switch {
		case err == nil:
			d.layoutRead, d.layout = true, l
		case errors.Is(err, fs.ErrEntryNotFound):
			d.layoutRead = true
		}
	}

	var mdErr error

	if d.metadata == nil {
		d.metadata, mdErr = image.ReadMetadata(ctx, rep, m)
	}

	c.mu.Lock()
	if c.entries == nil {
		c.entries = map[manifest.ID]*imageDetails{}
	}

	c.entries[m.ID] = &d
	c.mu.Unlock()

	return d.layout, d.metadata, mdErr //nolint:wrapcheck
}

// keepOnly forgets the images that are not among mans: they were deleted.
func (c *imageDetailsCache) keepOnly(mans []*snapshot.Manifest) {
	current := make(map[manifest.ID]bool, len(mans))
	for _, m := range mans {
		current[m.ID] = true
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	for id := range c.entries {
		if !current[id] {
			delete(c.entries, id)
		}
	}
}

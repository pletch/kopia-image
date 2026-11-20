package server

import (
	"context"
	"slices"
	"strings"
	"sync"

	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/snapshot"
	"github.com/kopia/kopia/snapshot/image"
)

// imageSpaceCache keeps the repository space of each source's images. It
// is calculated in the background, one source at a time, because it reads
// the chunk lists of every image of the source, and again when the source's
// images change.
type imageSpaceCache struct {
	mu sync.Mutex

	// +checklocks:mu
	entries map[snapshot.SourceInfo]*imageSpaceEntry

	// running is held by the calculation in progress.
	running sync.Mutex
}

type imageSpaceEntry struct {
	// key identifies the images the entry is for.
	key string

	done  bool
	space *image.Space
	err   error
}

// imageSetKey identifies a set of images, in any order.
func imageSetKey(mans []*snapshot.Manifest) string {
	ids := make([]string, 0, len(mans))
	for _, m := range mans {
		ids = append(ids, string(m.ID))
	}

	slices.Sort(ids)

	return strings.Join(ids, ",")
}

// get returns the space of a source's images if it was calculated for
// exactly these images, and otherwise starts calculating it and returns nil.
func (c *imageSpaceCache) get(ctx context.Context, rep repo.Repository, src snapshot.SourceInfo, mans []*snapshot.Manifest) *imageSpaceEntry {
	key := imageSetKey(mans)

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.entries == nil {
		c.entries = map[snapshot.SourceInfo]*imageSpaceEntry{}
	}

	// a failed calculation is reported until the images change
	if e := c.entries[src]; e != nil && e.key == key {
		if !e.done {
			return nil
		}

		return e
	}

	c.entries[src] = &imageSpaceEntry{key: key}

	mans = slices.Clone(mans)

	go func() {
		c.running.Lock()
		defer c.running.Unlock()

		// the images changed again while this waited
		if !c.wanted(src, key) {
			return
		}

		space, err := image.CalculateSpace(ctx, rep, mans)

		c.mu.Lock()
		defer c.mu.Unlock()

		if e := c.entries[src]; e != nil && e.key == key {
			e.done, e.space, e.err = true, space, err
		}
	}()

	return nil
}

func (c *imageSpaceCache) wanted(src snapshot.SourceInfo, key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	e := c.entries[src]

	return e != nil && e.key == key
}

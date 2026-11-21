package server

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"sync"

	"github.com/kopia/kopia/internal/atomicfile"
	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/content"
	"github.com/kopia/kopia/repo/manifest"
	"github.com/kopia/kopia/snapshot"
	"github.com/kopia/kopia/snapshot/image"
)

// imageSpaceFileName is the file in the repository's cache directory that
// keeps the calculated space of each source's images across server restarts.
const imageSpaceFileName = "image-space.json"

// imageSpaceCache keeps the repository space of each source's images. It
// is calculated in the background, one source at a time, because it reads
// the chunk lists of every image of the source, and again when the source's
// images change. What was calculated is saved to a file, so that a server
// started again shows it at once while the images are the same.
type imageSpaceCache struct {
	mu sync.Mutex

	// +checklocks:mu
	entries map[snapshot.SourceInfo]*imageSpaceEntry

	// file is where calculated space is saved, "" for nowhere; loaded is
	// whether it was read.
	// +checklocks:mu
	file string
	// +checklocks:mu
	loaded bool

	// running is held by the calculation in progress.
	running sync.Mutex

	// contents are the contents of each image, and sources the source of
	// each image in contents; sizes the stored size of each content. Images
	// and contents do not change, so a set of images that gained an image
	// reads only the new image and the sizes of its new contents.
	// +checklocks:running
	contents map[manifest.ID][]content.ID
	// +checklocks:running
	sources map[manifest.ID]snapshot.SourceInfo
	// +checklocks:running
	sizes map[content.ID]int64
}

type imageSpaceEntry struct {
	// key identifies the images the entry is for.
	key string

	done  bool
	space *image.Space
	err   error
}

// imageSpaceRecord is the saved space of one source's images.
type imageSpaceRecord struct {
	Source snapshot.SourceInfo `json:"source"`

	// Key identifies the images the space is for: their manifest IDs, which
	// are random, so a record is never used for another repository's images.
	Key string `json:"key"`

	StoredBytes    int64                 `json:"storedBytes"`
	ExclusiveBytes map[manifest.ID]int64 `json:"exclusiveBytes"`
}

type imageSpaceFile struct {
	Sources []imageSpaceRecord `json:"sources"`
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

// setFile sets where calculated space is saved, from file called before
// the first get only.
func (c *imageSpaceCache) setFile(file func() string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.loaded {
		c.file = file()
	}
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

	if !c.loaded {
		c.loaded = true
		c.loadLocked(ctx)
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

		space, err := c.calculateLocked(ctx, rep, src, mans)

		c.mu.Lock()
		defer c.mu.Unlock()

		if e := c.entries[src]; e != nil && e.key == key {
			e.done, e.space, e.err = true, space, err
			if err == nil {
				c.saveLocked(ctx)
			}
		}
	}()

	return nil
}

// calculateLocked calculates the space of a source's images, reusing what
// earlier calculations read.
//
// +checklocks:c.running
func (c *imageSpaceCache) calculateLocked(ctx context.Context, rep repo.Repository, src snapshot.SourceInfo, mans []*snapshot.Manifest) (*image.Space, error) {
	if c.contents == nil {
		c.contents = map[manifest.ID][]content.ID{}
		c.sources = map[manifest.ID]snapshot.SourceInfo{}
		c.sizes = map[content.ID]int64{}
	}

	current := map[manifest.ID]bool{}

	var all []content.ID

	for _, m := range mans {
		current[m.ID] = true

		ids, ok := c.contents[m.ID]
		if !ok {
			var err error

			if ids, err = image.Contents(ctx, rep, m); err != nil {
				return nil, err //nolint:wrapcheck
			}

			c.contents[m.ID], c.sources[m.ID] = ids, src
		}

		all = append(all, ids...)
	}

	// forget deleted images of the source
	for id, s := range c.sources {
		if s == src && !current[id] {
			delete(c.contents, id)
			delete(c.sources, id)
		}
	}

	if err := image.StoredSizes(ctx, rep, all, c.sizes); err != nil {
		return nil, err //nolint:wrapcheck
	}

	return image.SpaceOf(mans, c.contents, c.sizes), nil
}

func (c *imageSpaceCache) wanted(src snapshot.SourceInfo, key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	e := c.entries[src]

	return e != nil && e.key == key
}

// loadLocked reads the space saved by an earlier server. A file that cannot
// be read only means calculating again.
//
// +checklocks:c.mu
func (c *imageSpaceCache) loadLocked(ctx context.Context) {
	if c.file == "" {
		return
	}

	b, err := os.ReadFile(c.file)
	if err != nil {
		if !os.IsNotExist(err) {
			userLog(ctx).Debugf("unable to read saved image space: %v", err)
		}

		return
	}

	var f imageSpaceFile
	if err := json.Unmarshal(b, &f); err != nil {
		userLog(ctx).Debugf("unable to parse saved image space: %v", err)
		return
	}

	for _, r := range f.Sources {
		if c.entries[r.Source] != nil || r.ExclusiveBytes == nil {
			continue
		}

		c.entries[r.Source] = &imageSpaceEntry{
			key:   r.Key,
			done:  true,
			space: &image.Space{StoredBytes: r.StoredBytes, ExclusiveBytes: r.ExclusiveBytes},
		}
	}
}

// saveLocked saves the space calculated without error. Failing to save only
// means calculating again after a restart.
//
// +checklocks:c.mu
func (c *imageSpaceCache) saveLocked(ctx context.Context) {
	if c.file == "" {
		return
	}

	f := imageSpaceFile{Sources: []imageSpaceRecord{}}

	for src, e := range c.entries {
		if e.done && e.err == nil && e.space != nil {
			f.Sources = append(f.Sources, imageSpaceRecord{
				Source:         src,
				Key:            e.key,
				StoredBytes:    e.space.StoredBytes,
				ExclusiveBytes: e.space.ExclusiveBytes,
			})
		}
	}

	slices.SortFunc(f.Sources, func(a, b imageSpaceRecord) int {
		return strings.Compare(a.Source.String(), b.Source.String())
	})

	b, err := json.Marshal(f)
	if err != nil {
		userLog(ctx).Debugf("unable to encode image space: %v", err)
		return
	}

	if err := atomicfile.Write(c.file, bytes.NewReader(b)); err != nil {
		userLog(ctx).Debugf("unable to save image space: %v", err)
	}
}

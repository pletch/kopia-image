package imagecompare

import (
	"cmp"
	"context"
	"slices"

	"github.com/pkg/errors"

	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/snapshot/image"
	"github.com/kopia/kopia/snapshot/image/ntfsfs"
)

// Group names of changed clusters that no file of the new image holds.
const (
	GroupDeletedPrefix = "(deleted) "
	GroupFreeSpace     = "(free space)"
	GroupNoFile        = "(in use, no file)"
)

// mftScanCacheChunks is enough: the MFT is read once, in order.
const mftScanCacheChunks = 8

// attributeFiles groups the changed units of the sample by the files of the
// new image holding them, or for clusters freed since the old image, by the
// files of the old image that held them.
func attributeFiles(ctx context.Context, rep repo.Repository, ov, nv *image.Volume, sampled []*sampledChunk, opt Options) ([]GroupEstimate, error) {
	lcnOf := func(s *sampledChunk, u int) int64 {
		return (s.entry.Start + int64(u)*opt.UnitSize) / nv.ClusterSize
	}

	var lcns []int64

	for _, s := range sampled {
		for _, u := range s.changedUnits {
			lcns = append(lcns, lcnOf(s, u))
		}
	}

	l := &labeler{depth: opt.PathDepth, oldUses: map[int64]ntfsfs.ClusterUse{}}

	var err error

	if l.newUses, err = volumeClusterUses(ctx, rep, nv, lcns, StageNewMFT, opt); err != nil {
		return nil, errors.Wrap(err, "new image")
	}

	if freed := l.freed(); len(freed) > 0 && ov.Kind == image.VolumeKindNTFS && ov.ClusterSize == nv.ClusterSize {
		if l.oldUses, err = volumeClusterUses(ctx, rep, ov, freed, StageOldMFT, opt); err != nil {
			return nil, errors.Wrap(err, "old image")
		}
	}

	return groupChanges(sampled, opt.UnitSize, func(s *sampledChunk, u int) string { return l.label(lcnOf(s, u)) }), nil
}

// labeler names the group of a changed cluster.
type labeler struct {
	depth            int
	newUses, oldUses map[int64]ntfsfs.ClusterUse
}

// freed returns the clusters that are free in the new image.
func (l *labeler) freed() []int64 {
	var freed []int64

	for lcn, u := range l.newUses {
		if u.Free {
			freed = append(freed, lcn)
		}
	}

	return freed
}

func (l *labeler) label(lcn int64) string {
	u := l.newUses[lcn]

	switch {
	case u.Path != "":
		return groupOf(u.Path, l.depth)
	case u.Free && l.oldUses[lcn].Path != "":
		return GroupDeletedPrefix + groupOf(l.oldUses[lcn].Path, l.depth)
	case u.Free:
		return GroupFreeSpace
	default:
		return GroupNoFile
	}
}

// groupChanges totals the changed units of the sample by group, and shares
// each chunk among the groups that changed it, by changed units. Groups are
// sorted by chunk cost, largest first.
func groupChanges(sampled []*sampledChunk, unit int64, label func(*sampledChunk, int) string) []GroupEstimate {
	type totals struct {
		changed int64
		cost    float64
	}

	byGroup := map[string]*totals{}

	for _, s := range sampled {
		perChunk := map[string]int{}
		for _, u := range s.changedUnits {
			perChunk[label(s, u)]++
		}

		for g, n := range perChunk {
			t := byGroup[g]
			if t == nil {
				t = &totals{}
				byGroup[g] = t
			}

			t.changed += int64(n) * unit
			t.cost += float64(s.entry.Length) * float64(n) / float64(len(s.changedUnits))
		}
	}

	groups := make([]GroupEstimate, 0, len(byGroup))

	for g, t := range byGroup {
		groups = append(groups, GroupEstimate{Path: g, ChangedBytes: t.changed, ChunkCost: int64(t.cost)})
	}

	slices.SortFunc(groups, func(a, b GroupEstimate) int {
		if c := cmp.Compare(b.ChunkCost, a.ChunkCost); c != 0 {
			return c
		}

		return cmp.Compare(a.Path, b.Path)
	})

	return groups
}

func volumeClusterUses(ctx context.Context, rep repo.Repository, v *image.Volume, lcns []int64, stage string, opt Options) (map[int64]ntfsfs.ClusterUse, error) {
	vol, err := ntfsfs.Open(ctx, rep, v.ObjectID, v.EntryName, ntfsfs.Options{CacheChunks: mftScanCacheChunks})
	if err != nil {
		return nil, err //nolint:wrapcheck
	}
	defer vol.Close()

	uses, err := vol.ClusterUses(ctx, lcns, ntfsfs.ClusterUseOptions{Progress: func(done, total int64) {
		if opt.Progress != nil {
			opt.Progress(v.EntryName, stage, done, total)
		}
	}})

	return uses, errors.Wrap(err, "unable to map clusters to files")
}

// Package imagecompare measures what changed between two images of the same
// disk: which chunks hold data the older image does not have, how much new
// data smaller chunk sizes would have stored, and which files the changed
// clusters belong to.
//
// It compares chunks by position. The repository deduplicates against every
// chunk it holds, so the new data of a real run can be lower than the chunks
// counted here; the image's own recorded new data is the reference for the
// chunk size the images were taken with.
package imagecompare

import (
	"bytes"
	"context"
	"hash/maphash"
	"io"
	"math/rand/v2"
	"slices"
	"strings"
	"sync/atomic"

	"github.com/pkg/errors"
	"golang.org/x/sync/errgroup"

	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/object"
	"github.com/kopia/kopia/snapshot/image"
	"github.com/kopia/kopia/snapshot/image/imagerestore"
)

// Defaults of Options.
const (
	DefaultSample    = 1000
	DefaultParallel  = 8
	DefaultUnitSize  = 4 << 10
	DefaultPathDepth = 5
)

// DefaultBlockSizes are the chunk sizes estimated by default.
var DefaultBlockSizes = []int64{4 << 20, 2 << 20, 1 << 20, 512 << 10, 256 << 10, 128 << 10, 64 << 10}

// Stages reported to Options.Progress.
const (
	StageChunks = "fetching chunks"
	StageNewMFT = "scanning the new image's MFT"
	StageOldMFT = "scanning the old image's MFT"
)

// Options configure Compare.
type Options struct {
	// Sample is the number of new chunks fetched from both images and
	// compared; all of them when there are fewer.
	Sample int

	// Seed selects the sample, to repeat a comparison. Zero picks one.
	Seed uint64

	// Parallel is the number of chunk pairs fetched at once.
	Parallel int

	// UnitSize is the granularity at which chunks are compared, normally
	// the 4 KiB NTFS cluster size.
	UnitSize int64

	// BlockSizes are the chunk sizes to estimate the new data of. Each must
	// be a multiple of UnitSize that divides the images' chunk size.
	BlockSizes []int64

	// Files attributes the changed clusters of NTFS volumes to the files
	// that hold them, by scanning the MFT of both images.
	Files bool

	// PathDepth is the number of path components files are grouped by.
	PathDepth int

	// Progress, when set, is called as stages advance.
	Progress func(volume, stage string, done, total int64)
}

func (o Options) withDefaults() Options {
	if o.Sample <= 0 {
		o.Sample = DefaultSample
	}

	if o.Seed == 0 {
		o.Seed = rand.Uint64() //nolint:gosec
	}

	if o.Parallel <= 0 {
		o.Parallel = DefaultParallel
	}

	if o.UnitSize <= 0 {
		o.UnitSize = DefaultUnitSize
	}

	if len(o.BlockSizes) == 0 {
		o.BlockSizes = DefaultBlockSizes
	}

	if o.PathDepth <= 0 {
		o.PathDepth = DefaultPathDepth
	}

	return o
}

// Result is the comparison of two images.
type Result struct {
	Seed    uint64         `json:"seed"`
	Volumes []VolumeResult `json:"volumes"`

	// Unmatched lists the volumes found in only one of the images.
	Unmatched []string `json:"unmatched,omitempty"`
}

// VolumeResult compares the images of one partition.
type VolumeResult struct {
	EntryName string           `json:"entryName"`
	Kind      image.VolumeKind `json:"kind"`
	ChunkSize int64            `json:"chunkSize"`
	Size      int64            `json:"size"`

	// Chunks counts the chunks of the new image: Unchanged ones are the
	// same as at the same offset of the old image, Moved ones hold data the
	// old image has elsewhere, New ones hold data the old image does not.
	Chunks          int   `json:"chunks"`
	UnchangedChunks int   `json:"unchangedChunks"`
	MovedChunks     int   `json:"movedChunks"`
	NewChunks       int   `json:"newChunks"`
	NewChunkBytes   int64 `json:"newChunkBytes"`

	// SampledChunks of the new chunks, SampledBytes long, were compared.
	SampledChunks int   `json:"sampledChunks"`
	SampledBytes  int64 `json:"sampledBytes"`

	// ChangedBytes estimates the bytes that differ, counted in UnitSize
	// units, in the new chunks.
	ChangedBytes int64 `json:"changedBytes"`

	Blocks []BlockEstimate `json:"blocks"`

	// Groups attribute the changed data to folders, largest chunk cost
	// first; empty when files were not attributed.
	Groups []GroupEstimate `json:"groups,omitempty"`

	// FilesError says why files were not attributed, when they were asked for.
	FilesError string `json:"filesError,omitempty"`
}

// BlockEstimate is the new data the image would have stored with chunks of
// Size bytes.
type BlockEstimate struct {
	Size int64 `json:"size"`

	// SampleNewBytes is the new data found in the sample.
	SampleNewBytes int64 `json:"sampleNewBytes"`

	// NewBytes extrapolates it to all new chunks.
	NewBytes int64 `json:"newBytes"`

	// ListEntries is the number of entries of the volume's chunk list.
	ListEntries int64 `json:"listEntries"`
}

// GroupEstimate is the changed data of the files below a folder.
type GroupEstimate struct {
	Path string `json:"path"`

	// ChangedBytes estimates the bytes of the group's files that changed.
	ChangedBytes int64 `json:"changedBytes"`

	// ChunkCost estimates the new chunk data the group's changes caused:
	// each new chunk is shared among the groups that changed it, by
	// changed bytes.
	ChunkCost int64 `json:"chunkCost"`
}

// Compare compares two images of the same disk, older first.
func Compare(ctx context.Context, rep repo.Repository, older, newer *imagerestore.Snapshot, opt Options) (*Result, error) {
	opt = opt.withDefaults()

	for _, b := range opt.BlockSizes {
		if b <= 0 || b%opt.UnitSize != 0 {
			return nil, errors.Errorf("block size %v is not a multiple of the %v-byte comparison unit", b, opt.UnitSize)
		}
	}

	if older.Metadata.ChunkSize != newer.Metadata.ChunkSize {
		return nil, errors.Errorf("the images have different chunk sizes: %v and %v", older.Metadata.ChunkSize, newer.Metadata.ChunkSize)
	}

	res := &Result{Seed: opt.Seed}

	oldVolumes := map[int]*image.Volume{}
	for i := range older.Metadata.Volumes {
		v := &older.Metadata.Volumes[i]
		oldVolumes[v.PartitionNumber] = v
	}

	for i := range newer.Metadata.Volumes {
		nv := &newer.Metadata.Volumes[i]

		ov := oldVolumes[nv.PartitionNumber]
		if ov == nil {
			res.Unmatched = append(res.Unmatched, nv.EntryName+" (new image only)")
			continue
		}

		delete(oldVolumes, nv.PartitionNumber)

		vr, err := compareVolume(ctx, rep, ov, nv, newer.Metadata.ChunkSize, opt)
		if err != nil {
			return nil, errors.Wrapf(err, "volume %v", nv.EntryName)
		}

		res.Volumes = append(res.Volumes, *vr)
	}

	for _, ov := range oldVolumes {
		res.Unmatched = append(res.Unmatched, ov.EntryName+" (old image only)")
	}

	slices.Sort(res.Unmatched)

	return res, nil
}

// sampledChunk is the comparison of one chunk with the same chunk of the old
// image.
type sampledChunk struct {
	entry object.IndirectObjectEntry

	// changedUnits are the indexes of the units that differ
	changedUnits []int

	// per block size: hashes of the old and new blocks, and which new ones
	// are all zeros
	oldHashes, newHashes [][]uint64
	newZero              [][]bool
}

func compareVolume(ctx context.Context, rep repo.Repository, ov, nv *image.Volume, chunkSize int64, opt Options) (*VolumeResult, error) {
	vr := &VolumeResult{EntryName: nv.EntryName, Kind: nv.Kind, ChunkSize: chunkSize, Size: nv.Size}

	blockSizes := slices.DeleteFunc(slices.Clone(opt.BlockSizes), func(b int64) bool { return chunkSize%b != 0 })

	oldChunks, _, err := imagerestore.Chunks(ctx, rep, ov.ObjectID)
	if err != nil {
		return nil, errors.Wrap(err, "old image")
	}

	newChunks, _, err := imagerestore.Chunks(ctx, rep, nv.ObjectID)
	if err != nil {
		return nil, errors.Wrap(err, "new image")
	}

	oldAt := func(i int, e object.IndirectObjectEntry) *object.IndirectObjectEntry {
		if i < len(oldChunks) && oldChunks[i].Start == e.Start {
			return &oldChunks[i]
		}

		return nil
	}

	candidates := vr.classifyChunks(oldChunks, newChunks, oldAt)
	sample := pickSample(candidates, opt.Sample, opt.Seed)

	sampled, err := fetchSample(ctx, rep, nv.EntryName, newChunks, sample, oldAt, blockSizes, opt)
	if err != nil {
		return nil, err
	}

	vr.estimate(sampled, blockSizes, opt.UnitSize)

	if opt.Files && len(sampled) > 0 {
		vr.attribute(ctx, rep, ov, nv, sampled, opt)
	}

	return vr, nil
}

// classifyChunks counts the new image's chunks as unchanged, moved or new
// and returns the indexes of the new ones.
func (vr *VolumeResult) classifyChunks(oldChunks, newChunks []object.IndirectObjectEntry, oldAt func(int, object.IndirectObjectEntry) *object.IndirectObjectEntry) []int {
	oldIDs := make(map[object.ID]struct{}, len(oldChunks))
	for _, c := range oldChunks {
		oldIDs[c.Object] = struct{}{}
	}

	var candidates []int

	vr.Chunks = len(newChunks)

	for i, e := range newChunks {
		switch o := oldAt(i, e); {
		case o != nil && o.Object == e.Object:
			vr.UnchangedChunks++
		case hasID(oldIDs, e.Object):
			vr.MovedChunks++
		default:
			vr.NewChunks++
			vr.NewChunkBytes += e.Length

			candidates = append(candidates, i)
		}
	}

	return candidates
}

// scale extrapolates a byte count of the sample to all new chunks.
func (vr *VolumeResult) scale(n int64) int64 {
	if vr.SampledBytes == 0 {
		return 0
	}

	return int64(float64(n) * float64(vr.NewChunkBytes) / float64(vr.SampledBytes))
}

// estimate fills in the changed bytes and the new data of each block size.
func (vr *VolumeResult) estimate(sampled []*sampledChunk, blockSizes []int64, unit int64) {
	var changed int64

	for _, s := range sampled {
		vr.SampledChunks++
		vr.SampledBytes += s.entry.Length
		changed += int64(len(s.changedUnits)) * unit
	}

	vr.ChangedBytes = vr.scale(changed)

	for bi, b := range blockSizes {
		sampleNew := blockNewBytes(sampled, bi, b, unit)

		vr.Blocks = append(vr.Blocks, BlockEstimate{
			Size:           b,
			SampleNewBytes: sampleNew,
			NewBytes:       vr.scale(sampleNew),
			ListEntries:    (vr.Size + b - 1) / b,
		})
	}
}

// attribute fills in the folders that changed, or why they could not be
// found.
func (vr *VolumeResult) attribute(ctx context.Context, rep repo.Repository, ov, nv *image.Volume, sampled []*sampledChunk, opt Options) {
	if nv.Kind != image.VolumeKindNTFS || nv.ClusterSize <= 0 {
		vr.FilesError = "not an NTFS volume"
		return
	}

	groups, err := attributeFiles(ctx, rep, ov, nv, sampled, opt)
	if err != nil {
		vr.FilesError = err.Error()
		return
	}

	for i := range groups {
		groups[i].ChangedBytes = vr.scale(groups[i].ChangedBytes)
		groups[i].ChunkCost = vr.scale(groups[i].ChunkCost)
	}

	vr.Groups = groups
}

func hasID(ids map[object.ID]struct{}, id object.ID) bool {
	_, ok := ids[id]
	return ok
}

// pickSample returns up to n of the candidates, chosen at random with seed,
// in ascending order.
func pickSample(candidates []int, n int, seed uint64) []int {
	c := slices.Clone(candidates)
	if n >= len(c) {
		return c
	}

	r := rand.New(rand.NewPCG(seed, seed)) //nolint:gosec

	for i := range n {
		j := i + r.IntN(len(c)-i)
		c[i], c[j] = c[j], c[i]
	}

	c = c[:n]
	slices.Sort(c)

	return c
}

// fetchSample fetches the sampled chunks from both images and compares them.
func fetchSample(ctx context.Context, rep repo.Repository, volume string, newChunks []object.IndirectObjectEntry, sample []int,
	oldAt func(int, object.IndirectObjectEntry) *object.IndirectObjectEntry, blockSizes []int64, opt Options,
) ([]*sampledChunk, error) {
	out := make([]*sampledChunk, len(sample))

	var done atomic.Int64

	report := func() {
		if opt.Progress != nil {
			opt.Progress(volume, StageChunks, done.Load(), int64(len(sample)))
		}
	}

	report()

	seed := maphash.MakeSeed()

	eg, ctx := errgroup.WithContext(ctx)
	eg.SetLimit(opt.Parallel)

	for si, ci := range sample {
		eg.Go(func() error {
			e := newChunks[ci]

			newData, err := readChunk(ctx, rep, e)
			if err != nil {
				return err
			}

			// a chunk past the old image's end compares with zeros
			oldData := make([]byte, e.Length)

			if o := oldAt(ci, e); o != nil {
				d, err := readChunk(ctx, rep, *o)
				if err != nil {
					return err
				}

				copy(oldData, d)
			}

			out[si] = compareChunk(e, oldData, newData, blockSizes, opt.UnitSize, seed)

			done.Add(1)
			report()

			return nil
		})
	}

	if err := eg.Wait(); err != nil {
		return nil, err //nolint:wrapcheck
	}

	return out, nil
}

func readChunk(ctx context.Context, rep repo.Repository, e object.IndirectObjectEntry) ([]byte, error) {
	r, err := rep.OpenObject(ctx, e.Object)
	if err != nil {
		return nil, errors.Wrapf(err, "unable to open the chunk at offset %v", e.Start)
	}
	defer r.Close() //nolint:errcheck

	data := make([]byte, e.Length)

	if _, err := io.ReadFull(r, data); err != nil {
		return nil, errors.Wrapf(err, "unable to read the chunk at offset %v", e.Start)
	}

	return data, nil
}

func compareChunk(e object.IndirectObjectEntry, oldData, newData []byte, blockSizes []int64, unit int64, seed maphash.Seed) *sampledChunk {
	s := &sampledChunk{entry: e}

	for off := int64(0); off < int64(len(newData)); off += unit {
		end := min(off+unit, int64(len(newData)))
		if !bytes.Equal(oldData[off:end], newData[off:end]) {
			s.changedUnits = append(s.changedUnits, int(off/unit))
		}
	}

	for _, b := range blockSizes {
		var oh, nh []uint64

		var nz []bool

		for off := int64(0); off < int64(len(newData)); off += b {
			end := min(off+b, int64(len(newData)))
			oh = append(oh, maphash.Bytes(seed, oldData[off:end]))
			nh = append(nh, maphash.Bytes(seed, newData[off:end]))
			nz = append(nz, isZero(newData[off:end]))
		}

		s.oldHashes = append(s.oldHashes, oh)
		s.newHashes = append(s.newHashes, nh)
		s.newZero = append(s.newZero, nz)
	}

	return s
}

// blockNewBytes is the new data of the sample with blocks of size b (block
// size index bi): the blocks with a changed unit that are not all zeros and
// whose content is in no old block of the sample and no other new block
// counted before.
func blockNewBytes(sampled []*sampledChunk, bi int, b, unit int64) int64 {
	known := map[uint64]struct{}{}

	for _, s := range sampled {
		for _, h := range s.oldHashes[bi] {
			known[h] = struct{}{}
		}
	}

	var total int64

	for _, s := range sampled {
		unitsPerBlock := int(b / unit)
		changed := map[int]bool{}

		for _, u := range s.changedUnits {
			changed[u/unitsPerBlock] = true
		}

		for k, h := range s.newHashes[bi] {
			if !changed[k] || s.newZero[bi][k] {
				continue
			}

			if _, ok := known[h]; ok {
				continue
			}

			known[h] = struct{}{}

			start := int64(k) * b
			total += min(b, s.entry.Length-start)
		}
	}

	return total
}

func isZero(b []byte) bool {
	for _, x := range b {
		if x != 0 {
			return false
		}
	}

	return true
}

// groupOf returns the first depth components of a path.
func groupOf(path string, depth int) string {
	parts := strings.Split(path, `\`)
	if len(parts) > depth {
		parts = parts[:depth]
	}

	return strings.Join(parts, `\`)
}

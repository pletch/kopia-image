package imagerestore

import (
	"context"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"sync"

	"github.com/pkg/errors"
	"golang.org/x/sync/errgroup"

	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/blob"
	"github.com/kopia/kopia/repo/content"
	"github.com/kopia/kopia/repo/object"
	"github.com/kopia/kopia/snapshot/image"
)

const maxReportedProblems = 20

// CheckOptions configure Check.
type CheckOptions struct {
	// Sample is the fraction (0 to 1) of each volume's distinct chunks to
	// fetch and authenticate. 1 checks every chunk.
	Sample float64

	// Parallel is the number of chunks fetched at once.
	Parallel int

	// Seed selects the sample; 0 picks a random sample.
	Seed uint64

	// Progress, when set, receives the volume entry name and the chunks
	// fetched so far.
	Progress func(entry string, done, total int)
}

// CheckResult is the outcome of checking one volume image.
type CheckResult struct {
	Volume image.Volume

	// Chunks and DistinctChunks count the image's chunks.
	Chunks         int
	DistinctChunks int

	// PacksChecked reports whether every chunk's pack blob was checked to
	// exist and be long enough. It needs a direct repository connection.
	PacksChecked bool

	// FetchedChunks and FetchedBytes count the sampled chunks fetched.
	FetchedChunks int
	FetchedBytes  int64

	// Problems describes what failed, up to a limit; ProblemCount counts all.
	Problems     []string
	ProblemCount int
}

// OK reports whether the volume image passed.
func (r *CheckResult) OK() bool {
	return r.ProblemCount == 0
}

func (r *CheckResult) addProblemf(format string, args ...any) {
	r.ProblemCount++

	if len(r.Problems) < maxReportedProblems {
		r.Problems = append(r.Problems, fmt.Sprintf(format, args...))
	}
}

// Check verifies the volume images of a snapshot without writing anything:
// that every chunk is in the repository index, that every chunk's pack blob
// exists in storage and is long enough (on direct repository connections),
// and that a sample of distinct chunks can be fetched and authenticated.
//
// Fetched chunks may be served from this client's content cache; chunks
// written by this client are not cached for reading, so a check from the
// client that created the images reads them from storage.
func Check(ctx context.Context, rep repo.Repository, s *Snapshot, opt CheckOptions) ([]CheckResult, error) {
	if opt.Sample < 0 || opt.Sample > 1 {
		return nil, errors.Errorf("invalid sample fraction %v", opt.Sample)
	}

	if opt.Parallel <= 0 {
		opt.Parallel = defaultParallel
	}

	packs, err := listPacks(ctx, rep)
	if err != nil {
		return nil, err
	}

	seed := opt.Seed
	if seed == 0 {
		seed = rand.Uint64() //nolint:gosec
	}

	results := make([]CheckResult, 0, len(s.Metadata.Volumes))

	for i := range s.Metadata.Volumes {
		r, err := checkVolume(ctx, rep, &s.Metadata.Volumes[i], packs, opt, seed+uint64(i))
		if err != nil {
			return results, err
		}

		results = append(results, r)
	}

	return results, nil
}

// listPacks returns the pack blobs in storage, or nil when the repository is
// not a direct connection.
func listPacks(ctx context.Context, rep repo.Repository) (map[blob.ID]blob.Metadata, error) {
	dr, ok := rep.(repo.DirectRepository)
	if !ok {
		return nil, nil
	}

	packs, err := blob.ReadBlobMap(ctx, dr.BlobReader())

	return packs, errors.Wrap(err, "unable to list pack blobs")
}

func checkVolume(ctx context.Context, rep repo.Repository, v *image.Volume, packs map[blob.ID]blob.Metadata, opt CheckOptions, seed uint64) (CheckResult, error) {
	r := CheckResult{Volume: *v, PacksChecked: packs != nil}

	chunks, length, err := Chunks(ctx, rep, v.ObjectID)
	if err != nil {
		r.addProblemf("unable to read the chunk list: %v", err)
		return r, nil
	}

	r.Chunks = len(chunks)

	if length != v.Size {
		r.addProblemf("image is %v bytes, image.json says %v", length, v.Size)
	}

	// as many lookups at once as chunks are fetched in a restore
	lookups := opt.Parallel
	if len(chunks) > 0 {
		lookups = max(lookups, image.ParallelChunks(chunks[0].Length))
	}

	infos, err := image.VerifyObject(ctx, rep, v.ObjectID, lookups, nil)
	if err != nil {
		r.addProblemf("chunks missing from the index: %v", err)
		return r, nil
	}

	if packs != nil {
		checkPacks(infos, packs, &r)
	}

	groups := groupChunks(chunks)
	r.DistinctChunks = len(groups)

	sample := sampleGroups(groups, opt.Sample, seed)
	if err := fetchSample(ctx, rep, v.EntryName, sample, opt, &r); err != nil {
		return r, err
	}

	return r, nil
}

func checkPacks(infos map[content.ID]content.Info, packs map[blob.ID]blob.Metadata, r *CheckResult) {
	for cid, ci := range infos {
		bm, ok := packs[ci.PackBlobID]
		if !ok {
			r.addProblemf("content %v is in missing pack blob %v", cid, ci.PackBlobID)
			continue
		}

		if int64(ci.PackOffset)+int64(ci.PackedLength) > bm.Length {
			r.addProblemf("content %v lies beyond the end of pack blob %v", cid, ci.PackBlobID)
		}
	}
}

// sampleGroups picks ceil(fraction*n) distinct chunks, at least one when the
// fraction is positive.
func sampleGroups(groups []*chunkGroup, fraction float64, seed uint64) []*chunkGroup {
	if fraction <= 0 || len(groups) == 0 {
		return nil
	}

	n := min(len(groups), max(1, int(math.Ceil(fraction*float64(len(groups))))))
	perm := rand.New(rand.NewPCG(seed, seed)).Perm(len(groups)) //nolint:gosec

	out := make([]*chunkGroup, n)
	for i := range n {
		out[i] = groups[perm[i]]
	}

	return out
}

func fetchSample(ctx context.Context, rep repo.Repository, entry string, sample []*chunkGroup, opt CheckOptions, r *CheckResult) error {
	var (
		mu   sync.Mutex
		done int
	)

	eg, ctx := errgroup.WithContext(ctx)
	eg.SetLimit(opt.Parallel)

	for _, g := range sample {
		eg.Go(func() error {
			if err := ctx.Err(); err != nil {
				return err //nolint:wrapcheck
			}

			n, err := fetchChunk(ctx, rep, g.object)

			mu.Lock()
			defer mu.Unlock()

			switch {
			case err != nil:
				r.addProblemf("chunk at offset %v (%v): %v", g.offsets[0], g.object, err)
			case n != g.length:
				r.addProblemf("chunk at offset %v (%v) is %v bytes, the index says %v", g.offsets[0], g.object, n, g.length)
			default:
				r.FetchedChunks++
				r.FetchedBytes += n
			}

			done++

			if opt.Progress != nil {
				opt.Progress(entry, done, len(sample))
			}

			return nil
		})
	}

	return errors.Wrap(eg.Wait(), "check canceled")
}

func fetchChunk(ctx context.Context, rep repo.Repository, oid object.ID) (int64, error) {
	rd, err := rep.OpenObject(ctx, oid)
	if err != nil {
		return 0, errors.Wrap(err, "unable to open")
	}
	defer rd.Close() //nolint:errcheck

	n, err := io.Copy(io.Discard, rd)

	return n, errors.Wrap(err, "unable to read")
}

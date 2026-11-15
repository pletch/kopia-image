// Package image implements the platform-independent parts of block-level
// disk images: the image.json and layout.json formats, image snapshot source
// naming, the masked volume reader, and the restore script generator.
package image

import (
	"cmp"
	"math"
	"slices"

	"github.com/pkg/errors"
)

// Extent is the half-open byte range [Start, Start+Length) on a volume or disk.
type Extent struct {
	Start  int64 `json:"start"`
	Length int64 `json:"length"`
}

// End returns the offset of the first byte after the extent.
func (e Extent) End() int64 {
	return e.Start + e.Length
}

// NormalizeExtents returns extents covering the same bytes as the input,
// sorted by start, with empty extents dropped and overlapping or adjacent
// extents merged. The input is not modified.
func NormalizeExtents(in []Extent) ([]Extent, error) {
	sorted := make([]Extent, 0, len(in))

	for _, e := range in {
		if e.Start < 0 || e.Length < 0 || e.Start > math.MaxInt64-e.Length {
			return nil, errors.Errorf("invalid extent %+v", e)
		}

		if e.Length > 0 {
			sorted = append(sorted, e)
		}
	}

	slices.SortFunc(sorted, func(a, b Extent) int { return cmp.Compare(a.Start, b.Start) })

	var out []Extent

	for _, e := range sorted {
		if n := len(out); n > 0 && e.Start <= out[n-1].End() {
			out[n-1].Length = max(out[n-1].End(), e.End()) - out[n-1].Start
			continue
		}

		out = append(out, e)
	}

	return out, nil
}

// TotalLength returns the sum of the extent lengths.
func TotalLength(extents []Extent) int64 {
	var total int64

	for _, e := range extents {
		total += e.Length
	}

	return total
}

const (
	clustersPerBitmapByte = 8
	allClustersUsed       = 0xff
)

// FreeExtents returns the byte extents of the free clusters in an allocation
// bitmap that covers clusterCount clusters from cluster 0. The bitmap has the
// layout returned by FSCTL_GET_VOLUME_BITMAP: bit i%8 of byte i/8 is set when
// cluster i is in use. The result is normalized.
func FreeExtents(bitmap []byte, clusterCount, clusterSize int64) ([]Extent, error) {
	if clusterCount < 0 || clusterSize <= 0 || clusterCount > math.MaxInt64/clusterSize {
		return nil, errors.Errorf("invalid cluster count %v or size %v", clusterCount, clusterSize)
	}

	need := (clusterCount + clustersPerBitmapByte - 1) / clustersPerBitmapByte
	if int64(len(bitmap)) < need {
		return nil, errors.Errorf("bitmap has %v bytes, need %v for %v clusters", len(bitmap), need, clusterCount)
	}

	var (
		out       []Extent
		freeStart int64 = -1
	)

	endRun := func(end int64) {
		if freeStart >= 0 {
			out = append(out, Extent{Start: freeStart * clusterSize, Length: (end - freeStart) * clusterSize})
			freeStart = -1
		}
	}

	for c := int64(0); c < clusterCount; {
		b := bitmap[c/clustersPerBitmapByte]

		// whole bytes of free or used clusters are the common case on large volumes
		if c%clustersPerBitmapByte == 0 && clusterCount-c >= clustersPerBitmapByte && (b == 0 || b == allClustersUsed) {
			if b == allClustersUsed {
				endRun(c)
			} else if freeStart < 0 {
				freeStart = c
			}

			c += clustersPerBitmapByte

			continue
		}

		if b&(1<<(c%clustersPerBitmapByte)) != 0 {
			endRun(c)
		} else if freeStart < 0 {
			freeStart = c
		}

		c++
	}

	endRun(clusterCount)

	return out, nil
}

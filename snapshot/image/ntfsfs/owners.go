package ntfsfs

import (
	"context"
	"encoding/binary"
	"io"
	"slices"
	"strconv"
	"strings"
	"unicode/utf16"

	"github.com/pkg/errors"
	"www.velocidex.com/golang/go-ntfs/parser"
)

// $FILE_NAME attribute layout.
const (
	fileNameNamespaceOffset = 0x41
	fileNameLengthOffset    = 0x40
	fileNameOffset          = 0x42
	fileNameNamespaceDOS    = 2

	// maxPathDepth bounds the walk up a damaged directory tree.
	maxPathDepth = 256
)

// ClusterUse says what holds a cluster of a volume.
type ClusterUse struct {
	// Path is the path, relative to the volume root and separated by
	// backslashes, of the file or directory whose attributes reference the
	// cluster, such as `Users\me\AppData\Local\Temp\x.tmp` or `$MFT`. It is
	// empty when no in-use MFT record references the cluster.
	Path string

	// Free is set when $Bitmap marks the cluster free.
	Free bool
}

// ClusterUseOptions configure ClusterUses.
type ClusterUseOptions struct {
	// Progress, when set, receives the MFT records scanned so far.
	Progress func(done, total int64)
}

// ClusterUses reports what holds each of the clusters lcns, by reading
// $Bitmap and scanning every MFT record once. NTFS metadata files keep their
// names, e.g. `$MFT` or `$Extend\$UsnJrnl`. A cluster of a file with several
// hard links is reported under one of its names.
func (v *Volume) ClusterUses(ctx context.Context, lcns []int64, opt ClusterUseOptions) (map[int64]ClusterUse, error) {
	var mft io.ReaderAt

	err := v.call(func(ntfs *parser.NTFSContext) error {
		mft = ntfs.MFTReader
		return nil
	})
	if err != nil {
		return nil, err
	}

	return clusterUses(ctx, v.cr, v.cr.Size(), mft, lcns, opt)
}

func clusterUses(ctx context.Context, vol io.ReaderAt, volSize int64, mft io.ReaderAt, lcns []int64, opt ClusterUseOptions) (map[int64]ClusterUse, error) {
	c := &checker{vol: vol, volSize: volSize, mft: mft, r: &CheckResult{}}

	if !c.checkBootSector() || !c.readBitmaps() {
		return nil, errors.Errorf("unable to read the file system: %v", strings.Join(c.r.Problems, "; "))
	}

	s := &ownerScan{wanted: slices.Compact(slices.Sorted(slices.Values(lcns)))}

	s.owners = make([]int64, len(s.wanted))
	for i := range s.owners {
		s.owners[i] = -1
	}

	if err := s.scan(ctx, c, opt.Progress); err != nil {
		return nil, err
	}

	result := make(map[int64]ClusterUse, len(s.wanted))
	paths := map[int64]string{}

	for i, lcn := range s.wanted {
		u := ClusterUse{Free: !bitSet(c.volBitmap, lcn)}

		if owner := s.owners[i]; owner >= 0 {
			u.Path = s.pathOf(owner, paths)
		}

		result[lcn] = u
	}

	return result, nil
}

// ownerScan finds the MFT records referencing wanted clusters and keeps
// every record's name and parent to build their paths.
type ownerScan struct {
	wanted []int64 // sorted, unique
	owners []int64 // base MFT record per wanted cluster, or -1

	names   []string
	parents []int64
}

func (s *ownerScan) scan(ctx context.Context, c *checker, progress func(done, total int64)) error {
	size := mftSize(c.mft)
	if size <= 0 {
		return errors.New("the $MFT stream is empty")
	}

	records := size / c.recordSize
	s.names = make([]string, records)
	s.parents = make([]int64, records)

	buf := make([]byte, mftReadBatch-mftReadBatch%c.recordSize)

	for first := int64(0); first < records; {
		if err := ctx.Err(); err != nil {
			return errors.Wrap(err, "scan canceled")
		}

		n := min(int64(len(buf))/c.recordSize, records-first)
		b := buf[:n*c.recordSize]

		if _, err := c.mft.ReadAt(b, first*c.recordSize); err != nil && !errors.Is(err, io.EOF) {
			return errors.Wrapf(err, "unable to read MFT records %v-%v", first, first+n-1)
		}

		for i := range n {
			s.record(first+i, b[i*c.recordSize:(i+1)*c.recordSize])
		}

		first += n

		if progress != nil {
			progress(first, records)
		}
	}

	return nil
}

// record notes the name of an in-use base record and claims the wanted
// clusters its non-resident attributes reference. Damaged records are
// skipped: this is a report, not a check.
func (s *ownerScan) record(n int64, buf []byte) {
	rec, err := parseRecord(buf, n)
	if err != nil || rec.flags&recordFlagInUse == 0 {
		return
	}

	base := n
	if ref := int64(rec.baseRef & mftRefRecordMask); ref != 0 {
		base = ref
	} else {
		s.names[n], s.parents[n] = recordName(rec)
	}

	for i := range rec.attributes {
		a := &rec.attributes[i]
		if !a.nonResident {
			continue
		}

		runs, err := decodeRuns(a.runList)
		if err != nil {
			continue
		}

		for _, r := range runs {
			if !r.sparse {
				s.claim(base, r.lcn, r.length)
			}
		}
	}
}

// claim assigns the wanted clusters in [lcn, lcn+length) to record.
func (s *ownerScan) claim(record, lcn, length int64) {
	i, _ := slices.BinarySearch(s.wanted, lcn)

	for ; i < len(s.wanted) && s.wanted[i] < lcn+length; i++ {
		if s.owners[i] < 0 {
			s.owners[i] = record
		}
	}
}

// pathOf returns the path of a record, caching the paths of directories.
func (s *ownerScan) pathOf(n int64, cache map[int64]string) string {
	if p, ok := cache[n]; ok {
		return p
	}

	var parts []string

	cur := n
	for depth := 0; cur != rootMFTEntry; depth++ {
		if p, ok := cache[cur]; ok {
			parts = append(parts, p)
			break
		}

		if depth >= maxPathDepth || cur < 0 || cur >= int64(len(s.names)) || s.names[cur] == "" {
			parts = append(parts, "<MFT record "+strconv.FormatInt(cur, 10)+">")
			break
		}

		parts = append(parts, s.names[cur])
		cur = s.parents[cur]
	}

	slices.Reverse(parts)
	p := strings.Join(parts, `\`)

	if n != rootMFTEntry {
		cache[n] = p
	}

	return p
}

// recordName returns a base record's long name and parent record, preferring
// a Win32 or POSIX name over a DOS 8.3 one.
func recordName(rec *mftRecord) (string, int64) {
	var (
		name   string
		parent int64
	)

	for i := range rec.attributes {
		a := &rec.attributes[i]
		if a.typ != attrTypeFileName || a.nonResident || len(a.value) < fileNameOffset {
			continue
		}

		nameLen := int(a.value[fileNameLengthOffset])
		if len(a.value) < fileNameOffset+2*nameLen {
			continue
		}

		u := make([]uint16, nameLen)
		for j := range u {
			u[j] = binary.LittleEndian.Uint16(a.value[fileNameOffset+2*j:])
		}

		name = string(utf16.Decode(u))
		parent = int64(binary.LittleEndian.Uint64(a.value) & mftRefRecordMask)

		if a.value[fileNameNamespaceOffset] != fileNameNamespaceDOS {
			break
		}
	}

	return name, parent
}

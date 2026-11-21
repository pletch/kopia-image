package image

import (
	"cmp"
	"math"
	"slices"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/pkg/errors"
)

// GPT partition type GUIDs that image creation and restore treat specially.
const (
	PartitionTypeEFISystem   = "c12a7328-f81f-11d2-ba4b-00a0c93ec93b"
	PartitionTypeMSR         = "e3c9e316-0b5c-4db8-817d-f92df00215ae"
	PartitionTypeBasicData   = "ebd0a0a2-b9e5-4433-87c0-68b6b72699c7"
	PartitionTypeWinRecovery = "de94bba4-06d1-4d40-a16a-bfd50179d6ac"
)

const (
	minSectorSize = 512
	maxSectorSize = 64 << 10

	maxGPTPartitions   = 128
	maxGPTNameUTF16Len = 36
	gptEntryArrayBytes = maxGPTPartitions * 128 // 128 entries of 128 bytes
)

// gptEntrySectors returns how many sectors one partition entry array takes:
// 32 with 512-byte sectors, 4 with 4096-byte sectors.
func (l *Layout) gptEntrySectors() int64 {
	if l.SectorSize <= 0 {
		return 0 // Validate rejects such layouts
	}

	return (gptEntryArrayBytes + l.SectorSize - 1) / l.SectorSize
}

// gptPrimaryBytes returns the size of the protective MBR, primary header and
// entry array at the start of the disk.
func (l *Layout) gptPrimaryBytes() int64 {
	return (2 + l.gptEntrySectors()) * l.SectorSize //nolint:mnd
}

// gptBackupBytes returns the size of the backup entry array and header at the
// end of the disk.
func (l *Layout) gptBackupBytes() int64 {
	return (1 + l.gptEntrySectors()) * l.SectorSize
}

// Layout is the content of layout.json: the GPT partition table of an imaged
// disk, enough to recreate it on a restore target.
type Layout struct {
	// DiskGUID is the GPT disk GUID in canonical form.
	DiskGUID string `json:"diskGuid"`

	// SectorSize is the logical sector size in bytes. A restore target must
	// have the same logical sector size.
	SectorSize int64 `json:"sectorSize"`

	// DiskSize is the size of the source disk in bytes.
	DiskSize int64 `json:"diskSize"`

	// Partitions holds the GPT entries in use, ordered by Number.
	Partitions []Partition `json:"partitions"`
}

// Partition is one GPT partition entry.
type Partition struct {
	// Number is the 1-based index of the entry in the GPT partition array.
	Number int `json:"number"`

	// TypeGUID and UniqueGUID are in canonical form.
	TypeGUID   string `json:"typeGuid"`
	UniqueGUID string `json:"uniqueGuid"`

	// Offset and Length are in bytes and are multiples of the sector size.
	Offset int64 `json:"offset"`
	Length int64 `json:"length"`

	// Name is the GPT partition name.
	Name string `json:"name"`

	// Attributes is the 64-bit GPT attribute field. It is encoded as a JSON
	// string because bits 60-63 are commonly set and would lose precision in
	// readers that decode numbers as float64.
	Attributes uint64 `json:"attributes,string"`
}

// End returns the byte offset just past the partition.
func (p Partition) End() int64 {
	return p.Offset + p.Length
}

// unsupportedPartitionTypes are GPT partition types of disks that image
// creation refuses, because their volumes do not map to single partitions.
var unsupportedPartitionTypes = map[string]string{
	"5808c8aa-7e8f-42e0-85d2-e1e90434cfb3": "dynamic disk (LDM metadata)",
	"af9b60a0-1431-4f62-bc68-3311714a69ad": "dynamic disk (LDM data)",
	"e75caf8f-f680-4cee-afa3-b001e56efc2d": "Storage Spaces",
}

// ErrUnsupportedLayout is returned for disks that image creation refuses.
var ErrUnsupportedLayout = errors.New("unsupported disk layout")

// CheckSupported returns an error wrapping ErrUnsupportedLayout if the disk
// is a dynamic disk or part of a Storage Spaces pool.
func (l *Layout) CheckSupported() error {
	for _, p := range l.Partitions {
		if what, ok := unsupportedPartitionTypes[p.TypeGUID]; ok {
			return errors.Wrapf(ErrUnsupportedLayout, "partition %v is %v", p.Number, what)
		}
	}

	return nil
}

// Partition returns the partition with the given GPT entry number.
func (l *Layout) Partition(number int) (Partition, bool) {
	for _, p := range l.Partitions {
		if p.Number == number {
			return p, true
		}
	}

	return Partition{}, false
}

// MinTargetSize returns the smallest restore target, in bytes, that can hold
// every partition at its original offset plus the backup GPT.
func (l *Layout) MinTargetSize() int64 {
	end := l.gptPrimaryBytes()

	for _, p := range l.Partitions {
		end = max(end, p.End())
	}

	return end + l.gptBackupBytes()
}

// Validate checks that the layout describes a GPT disk that a restore can
// recreate exactly.
func (l *Layout) Validate() error {
	if err := requireCanonicalGUID("disk GUID", l.DiskGUID); err != nil {
		return err
	}

	if l.SectorSize < minSectorSize || l.SectorSize > maxSectorSize || l.SectorSize&(l.SectorSize-1) != 0 {
		return errors.Errorf("invalid sector size %v", l.SectorSize)
	}

	if l.DiskSize <= 0 || l.DiskSize%l.SectorSize != 0 {
		return errors.Errorf("invalid disk size %v", l.DiskSize)
	}

	if len(l.Partitions) == 0 {
		return errors.New("no partitions")
	}

	for i, p := range l.Partitions {
		if err := l.validatePartition(p); err != nil {
			return errors.Wrapf(err, "partition %v", p.Number)
		}

		if i > 0 && p.Number <= l.Partitions[i-1].Number {
			return errors.Errorf("partitions are not ordered by number at partition %v", p.Number)
		}
	}

	byOffset := slices.Clone(l.Partitions)
	slices.SortFunc(byOffset, func(a, b Partition) int { return cmp.Compare(a.Offset, b.Offset) })

	for i := 1; i < len(byOffset); i++ {
		if byOffset[i].Offset < byOffset[i-1].End() {
			return errors.Errorf("partitions %v and %v overlap", byOffset[i-1].Number, byOffset[i].Number)
		}
	}

	return nil
}

func (l *Layout) validatePartition(p Partition) error {
	if p.Number < 1 || p.Number > maxGPTPartitions {
		return errors.Errorf("invalid partition number %v", p.Number)
	}

	if err := requireCanonicalGUID("type GUID", p.TypeGUID); err != nil {
		return err
	}

	if err := requireCanonicalGUID("unique GUID", p.UniqueGUID); err != nil {
		return err
	}

	if p.Offset < l.gptPrimaryBytes() || p.Offset%l.SectorSize != 0 {
		return errors.Errorf("invalid offset %v", p.Offset)
	}

	if p.Length <= 0 || p.Length%l.SectorSize != 0 || p.Offset > math.MaxInt64-p.Length {
		return errors.Errorf("invalid length %v", p.Length)
	}

	if p.End() > l.DiskSize-l.gptBackupBytes() {
		return errors.Errorf("partition ends at %v, beyond the usable end of the disk", p.End())
	}

	return validatePartitionName(p.Name)
}

func validatePartitionName(name string) error {
	if !utf8.ValidString(name) {
		return errors.New("partition name is not valid UTF-8")
	}

	runes := []rune(name)

	if len(utf16.Encode(runes)) > maxGPTNameUTF16Len {
		return errors.Errorf("partition name %q is longer than %v UTF-16 code units", name, maxGPTNameUTF16Len)
	}

	if slices.ContainsFunc(runes, unicode.IsControl) {
		return errors.Errorf("partition name %q contains a control character", name)
	}

	return nil
}

func requireCanonicalGUID(what, s string) error {
	g, err := CanonicalGUID(s)
	if err != nil {
		return errors.Wrap(err, what)
	}

	if g != s {
		return errors.Errorf("%v %q is not in canonical form %q", what, s, g)
	}

	return nil
}

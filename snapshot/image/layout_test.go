package image

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/repo/object"
)

func testLayout() *Layout {
	return &Layout{
		DiskGUID:   testDiskGUID,
		SectorSize: 512,
		DiskSize:   128035676160,
		Partitions: []Partition{
			{
				Number: 1, TypeGUID: PartitionTypeEFISystem, UniqueGUID: "11111111-2222-4333-8444-555555555551",
				Offset: 1 << 20, Length: 100 << 20, Name: "EFI system partition", Attributes: 1 << 63,
			},
			{
				Number: 2, TypeGUID: PartitionTypeMSR, UniqueGUID: "11111111-2222-4333-8444-555555555552",
				Offset: 101 << 20, Length: 16 << 20, Name: "Microsoft reserved partition",
			},
			{
				Number: 3, TypeGUID: PartitionTypeBasicData, UniqueGUID: "11111111-2222-4333-8444-555555555553",
				Offset: 117 << 20, Length: 100 << 30, Name: "Basic data partition",
			},
			{
				Number: 4, TypeGUID: PartitionTypeWinRecovery, UniqueGUID: "11111111-2222-4333-8444-555555555554",
				Offset: 117<<20 + 100<<30, Length: 650 << 20, Name: "Tim's recovery", Attributes: 1<<63 | 1,
			},
		},
	}
}

func mustParseObjectID(t *testing.T, s string) object.ID {
	t.Helper()

	oid, err := object.ParseID(s)
	require.NoError(t, err)

	return oid
}

func testMetadata(t *testing.T) *Metadata {
	t.Helper()

	return &Metadata{
		FormatVersion: FormatVersion,
		ChunkSize:     DefaultChunkSize,
		DiskGUID:      testDiskGUID,
		StartTime:     time.Date(2026, 11, 15, 3, 0, 0, 0, time.UTC),
		EndTime:       time.Date(2026, 11, 15, 3, 4, 30, 0, time.UTC),
		Volumes: []Volume{
			{
				EntryName: VolumeEntryName(1, "esp"), PartitionNumber: 1, Kind: VolumeKindRaw,
				ObjectID: mustParseObjectID(t, "Ix0123456789abcdef0123456789abcd01"), Size: 100 << 20,
			},
			{
				EntryName: VolumeEntryName(3, "C"), PartitionNumber: 3, Kind: VolumeKindNTFS,
				ObjectID: mustParseObjectID(t, "Ix0123456789abcdef0123456789abcd03"), Size: 100 << 30,
				ClusterSize: 4096, SerialNumber: "8a1c2e3f4b5d6a7e", FreeBytes: 40 << 30,
				UnstableExtents: []Extent{{1 << 30, 16 << 30}},
			},
			{
				EntryName: VolumeEntryName(4, "recovery"), PartitionNumber: 4, Kind: VolumeKindRaw,
				ObjectID: mustParseObjectID(t, "Ix0123456789abcdef0123456789abcd04"), Size: 650 << 20,
			},
		},
	}
}

func TestLayout_jsonRoundTrip(t *testing.T) {
	l := testLayout()
	require.NoError(t, l.Validate())

	b, err := json.Marshal(l)
	require.NoError(t, err)
	require.Contains(t, string(b), `"attributes":"9223372036854775809"`)

	var got Layout

	require.NoError(t, json.Unmarshal(b, &got))
	require.Equal(t, l, &got)
}

func TestLayout_MinTargetSize(t *testing.T) {
	require.Equal(t, int64(117<<20+100<<30+650<<20+33*512), testLayout().MinTargetSize())
}

func TestLayout_Validate_invalid(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(l *Layout)
		want   string
	}{
		{"disk guid", func(l *Layout) { l.DiskGUID = "x" }, "disk GUID"},
		{"disk guid uppercase", func(l *Layout) { l.DiskGUID = strings.ToUpper(testDiskGUID) }, "canonical"},
		{"sector size", func(l *Layout) { l.SectorSize = 1000 }, "sector size"},
		{"disk size", func(l *Layout) { l.DiskSize = 1001 }, "disk size"},
		{"no partitions", func(l *Layout) { l.Partitions = nil }, "no partitions"},
		{"number", func(l *Layout) { l.Partitions[0].Number = 0 }, "partition number"},
		{"order", func(l *Layout) { l.Partitions[1].Number = 1 }, "ordered"},
		{"type guid", func(l *Layout) { l.Partitions[0].TypeGUID = "" }, "type GUID"},
		{"unaligned offset", func(l *Layout) { l.Partitions[0].Offset++ }, "offset"},
		{"offset in primary gpt", func(l *Layout) { l.Partitions[0].Offset = 512 }, "offset"},
		{"zero length", func(l *Layout) { l.Partitions[0].Length = 0 }, "length"},
		{"beyond disk", func(l *Layout) { l.Partitions[3].Length = l.DiskSize }, "beyond"},
		{"overlap", func(l *Layout) { l.Partitions[1].Offset = l.Partitions[0].Offset + 512 }, "overlap"},
		{"name too long", func(l *Layout) { l.Partitions[0].Name = strings.Repeat("a", 37) }, "longer"},
		{"name control char", func(l *Layout) { l.Partitions[0].Name = "a\nb" }, "control"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := testLayout()
			tc.mutate(l)
			require.ErrorContains(t, l.Validate(), tc.want)
		})
	}
}

func TestMetadata_jsonRoundTrip(t *testing.T) {
	md := testMetadata(t)
	require.NoError(t, md.Validate(testLayout()))

	b, err := json.Marshal(md)
	require.NoError(t, err)

	var got Metadata

	require.NoError(t, json.Unmarshal(b, &got))
	require.Equal(t, md, &got)
}

func TestMetadata_Validate_invalid(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(m *Metadata)
		want   string
	}{
		{"version", func(m *Metadata) { m.FormatVersion = 2 }, "version"},
		{"chunk size", func(m *Metadata) { m.ChunkSize = 1000 }, "chunk size"},
		{"disk guid", func(m *Metadata) { m.DiskGUID = "11111111-2222-4333-8444-555555555555" }, "does not match"},
		{"no volumes", func(m *Metadata) { m.Volumes = nil }, "no volumes"},
		{"unknown partition", func(m *Metadata) { m.Volumes[0].PartitionNumber = 9 }, "not in the layout"},
		{"entry name", func(m *Metadata) { m.Volumes[0].EntryName = "p2-esp.img" }, "entry name"},
		{"kind", func(m *Metadata) { m.Volumes[0].Kind = "zfs" }, "kind"},
		{"object id", func(m *Metadata) { m.Volumes[0].ObjectID = object.EmptyID }, "object ID"},
		{"too large", func(m *Metadata) { m.Volumes[0].Size = 101 << 20 }, "does not fit"},
		{"order", func(m *Metadata) { m.Volumes[0], m.Volumes[1] = m.Volumes[1], m.Volumes[0] }, "ordered"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := testMetadata(t)
			tc.mutate(m)
			require.ErrorContains(t, m.Validate(testLayout()), tc.want)
		})
	}
}

func TestVolumeEntryName(t *testing.T) {
	require.Equal(t, "p3-C.img", VolumeEntryName(3, "C"))
	require.Equal(t, "p3-C.bitmap", BitmapEntryName(3, "C"))
	require.Equal(t, "p1-esp.img", VolumeEntryName(1, "esp"))
	require.Equal(t, "p5-part.img", VolumeEntryName(5, ""))
	require.Equal(t, "p2-a_b__c.img", VolumeEntryName(2, "a b/.c"))
}

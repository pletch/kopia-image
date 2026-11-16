package disksource

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/snapshot/image"
)

func testLayout() *image.Layout {
	return &image.Layout{
		DiskGUID:   "0f6ab3f2-1c3d-4e5f-8a9b-0c1d2e3f4a5b",
		SectorSize: 512,
		DiskSize:   256 << 30,
		Partitions: []image.Partition{
			{Number: 1, TypeGUID: image.PartitionTypeEFISystem, UniqueGUID: "11111111-2222-4333-8444-555555555551", Offset: 1 << 20, Length: 100 << 20, Name: "EFI system partition"},
			{Number: 2, TypeGUID: image.PartitionTypeMSR, UniqueGUID: "11111111-2222-4333-8444-555555555552", Offset: 101 << 20, Length: 16 << 20, Name: "Microsoft reserved partition"},
			{Number: 3, TypeGUID: image.PartitionTypeBasicData, UniqueGUID: "11111111-2222-4333-8444-555555555553", Offset: 117 << 20, Length: 100 << 30, Name: "Basic data partition"},
			{Number: 4, TypeGUID: image.PartitionTypeWinRecovery, UniqueGUID: "11111111-2222-4333-8444-555555555554", Offset: 117<<20 + 100<<30, Length: 1 << 30, Name: "Recovery"},
		},
	}
}

func windowsVolumes() []VolumeInfo {
	return []VolumeInfo{
		{GUIDPath: `\\?\Volume{c}\`, FileSystem: "NTFS", MountPoints: []string{`C:\`}, Offset: 117 << 20, Length: 100 << 30},
		{GUIDPath: `\\?\Volume{esp}\`, FileSystem: "FAT32", Offset: 1 << 20, Length: 100 << 20},
		{GUIDPath: `\\?\Volume{re}\`, FileSystem: "NTFS", Offset: 117<<20 + 100<<30, Length: 1 << 30},
		{GUIDPath: `\\?\Volume{other}\`, FileSystem: "NTFS", MountPoints: []string{`D:\`}, Offset: 1 << 40, Length: 1 << 30},
	}
}

func TestBuildPlan_windowsSystemDisk(t *testing.T) {
	plan, err := BuildPlan(0, testLayout(), windowsVolumes())
	require.NoError(t, err)

	type summary struct {
		number int
		kind   image.VolumeKind
		entry  string
	}

	var got []summary

	for i := range plan.Partitions {
		pp := &plan.Partitions[i]
		require.Empty(t, pp.Warning, "partition %v", pp.Partition.Number)

		got = append(got, summary{pp.Partition.Number, pp.Kind, pp.EntryName()})
	}

	require.Equal(t, []summary{
		{1, image.VolumeKindRaw, "p1-esp.img"},
		{3, image.VolumeKindNTFS, "p3-C.img"},
		{4, image.VolumeKindRaw, "p4-recovery.img"},
	}, got)

	require.Equal(t, `\\?\Volume{c}\`, plan.Partitions[1].Volume.GUIDPath)
	require.Len(t, plan.Skipped, 1)
	require.Equal(t, 2, plan.Skipped[0].Number)

	var sb strings.Builder

	plan.Print(&sb)
	require.Contains(t, sb.String(), "p3-C.img (NTFS from a shadow copy of C:\\)")
	require.Contains(t, sb.String(), "not imaged, recreated on restore")
}

func TestBuildPlan_dataVolumes(t *testing.T) {
	cases := []struct {
		name        string
		volume      *VolumeInfo
		wantKind    image.VolumeKind
		wantLabel   string
		wantWarning string
	}{
		{"ntfs mounted at folder", &VolumeInfo{FileSystem: "NTFS", MountPoints: []string{`C:\mnt\data\`}}, image.VolumeKindNTFS, "data", ""},
		{"ntfs lowercase letter", &VolumeInfo{FileSystem: "ntfs", MountPoints: []string{`e:\`}}, image.VolumeKindNTFS, "E", ""},
		{"exfat", &VolumeInfo{FileSystem: "exFAT", MountPoints: []string{`F:\`}}, image.VolumeKindRaw, "F", "read raw"},
		{"no volume", nil, image.VolumeKindRaw, "data", "without a mounted volume"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := testLayout()

			var volumes []VolumeInfo

			if tc.volume != nil {
				v := *tc.volume
				v.GUIDPath = `\\?\Volume{x}\`
				v.Offset = l.Partitions[2].Offset
				v.Length = l.Partitions[2].Length
				volumes = append(volumes, v)
			}

			plan, err := BuildPlan(1, l, volumes)
			require.NoError(t, err)

			pp := plan.Partitions[1]
			require.Equal(t, 3, pp.Partition.Number)
			require.Equal(t, tc.wantKind, pp.Kind)
			require.Equal(t, tc.wantLabel, pp.Label)

			if tc.wantWarning == "" {
				require.Empty(t, pp.Warning)
			} else {
				require.Contains(t, pp.Warning, tc.wantWarning)
			}
		})
	}
}

func TestBuildPlan_unknownPartitionType(t *testing.T) {
	l := testLayout()
	l.Partitions[3].TypeGUID = "bfbfafe7-a34f-448a-9a5b-6213eb736c22" // an OEM partition

	plan, err := BuildPlan(0, l, nil)
	require.NoError(t, err)
	require.Equal(t, "p4-part.img", plan.Partitions[2].EntryName())
	require.Contains(t, plan.Partitions[2].Warning, "not recognized")
}

func TestBuildPlan_refused(t *testing.T) {
	l := testLayout()
	volumes := windowsVolumes()
	volumes[0].FileSystem = "ReFS"

	_, err := BuildPlan(0, l, volumes)
	require.ErrorIs(t, err, image.ErrUnsupportedLayout)

	l = testLayout()
	l.Partitions[2].TypeGUID = "af9b60a0-1431-4f62-bc68-3311714a69ad"

	_, err = BuildPlan(0, l, nil)
	require.ErrorIs(t, err, image.ErrUnsupportedLayout)

	volumes = windowsVolumes()
	volumes[0].Length++

	_, err = BuildPlan(0, testLayout(), volumes)
	require.ErrorContains(t, err, "larger than its partition")

	l = testLayout()
	l.Partitions = l.Partitions[1:2]

	_, err = BuildPlan(0, l, nil)
	require.ErrorContains(t, err, "no partitions to image")
}

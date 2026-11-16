package imagerestore_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/snapshot/image/imagerestore"
)

func TestDiskInfo_TargetProblem(t *testing.T) {
	const (
		gib    = int64(1) << 30
		sector = int64(512)
	)

	cases := []struct {
		name string
		disk imagerestore.DiskInfo
		want string
	}{
		{"blank disk of the same size", imagerestore.DiskInfo{Size: 100 * gib, SectorSize: sector}, ""},
		{"larger disk with old volumes", imagerestore.DiskInfo{Size: 200 * gib, SectorSize: sector, Volumes: []imagerestore.DiskVolume{{FileSystem: "NTFS", Label: "Windows", MountPoints: []string{`D:\`}}}}, ""},
		{"recovery media", imagerestore.DiskInfo{Size: 200 * gib, SectorSize: sector, Volumes: []imagerestore.DiskVolume{{FileSystem: "FAT32", Label: "kopiare"}}}, "the recovery media"},
		{"too small", imagerestore.DiskInfo{Size: 50 * gib, SectorSize: sector}, "too small, the image needs 107.4 GB"},
		{"4K sectors", imagerestore.DiskInfo{Size: 200 * gib, SectorSize: 4096}, "has 4096-byte sectors, the image needs 512"},
		{"holds the repository", imagerestore.DiskInfo{Size: 200 * gib, SectorSize: sector, Volumes: []imagerestore.DiskVolume{{FileSystem: "NTFS", MountPoints: []string{`E:\`}}}}, `holds e:\repo`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, tc.disk.TargetProblem(100*gib, sector, `e:\repo`))
		})
	}
}

func TestDiskInfo_Describe(t *testing.T) {
	d := imagerestore.DiskInfo{Path: `\.\PhysicalDrive2`, Model: "Samsung SSD 860 EVO 1TB", Bus: "SATA", Size: 1000204886016}
	require.Equal(t, `\.\PhysicalDrive2  1 TB  Samsung SSD 860 EVO 1TB  (SATA)`, d.Describe())
	require.Equal(t, []string{"no volumes"}, d.DescribeVolumes())

	d.Volumes = []imagerestore.DiskVolume{{FileSystem: "NTFS", Label: "Data", MountPoints: []string{`D:\`}}, {FileSystem: "FAT32"}}
	require.Equal(t, []string{`NTFS "Data" at D:\`, "FAT32"}, d.DescribeVolumes())
}

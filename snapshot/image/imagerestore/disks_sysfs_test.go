package imagerestore

import (
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/require"
)

// testSysFS is a computer with:
//
//   - sda: the system disk, with the ESP and the root file system
//   - sdb and sdc: a btrfs RAID1 holding the repository, mounted from sdb1,
//     with sdc a member as a whole disk
//   - sdd: LVM on LUKS on sdd1, mounted from /dev/mapper/vg-data
//   - sde: swap
//   - sdf: a member of an unmounted RAID array
//   - sdg: blank
//   - sdaa: a disk whose name starts with sda, mounted at a path with a space
//   - nvme0n1 with a mounted partition, and the blank nvme0n10
//   - loop0, which is not a disk
func testSysFS() fstest.MapFS {
	m := fstest.MapFS{
		"proc/mounts": {Data: []byte(`sysfs /sys sysfs rw 0 0
/dev/sda2 / ext4 rw 0 0
/dev/sda1 /boot/efi vfat rw 0 0
/dev/sdb1 /mnt/repo btrfs rw 0 0
/dev/mapper/vg-data /data ext4 rw 0 0
/dev/sdaa1 /mnt/my\040disk ext4 rw 0 0
/dev/nvme0n1p1 /home xfs rw 0 0
`)},
		"proc/swaps": {Data: []byte(`Filename	Type	Size	Used	Priority
/dev/sde1	partition	1048572	0	-2
`)},
		"sys/fs/btrfs/features/raid1c34":      {Data: []byte("0\n")},
		"sys/fs/btrfs/0d2a-uuid/devices/sdb1": {},
		"sys/fs/btrfs/0d2a-uuid/devices/sdc":  {},

		"sys/block/sdd/sdd1/holders/dm-0": {},
		"sys/block/dm-0/holders/dm-1":     {},
		"sys/block/sdf/sdf1/holders/md0":  {},
	}

	disk := func(name string, parts ...string) {
		m["sys/block/"+name+"/size"] = &fstest.MapFile{Data: []byte("2097152\n")}
		m["sys/block/"+name+"/queue/logical_block_size"] = &fstest.MapFile{Data: []byte("512\n")}

		for _, p := range parts {
			m["sys/block/"+name+"/"+p+"/partition"] = &fstest.MapFile{Data: []byte("1\n")}
		}
	}

	disk("sda", "sda1", "sda2")
	disk("sdb", "sdb1")
	disk("sdc")
	disk("sdd", "sdd1")
	disk("sde", "sde1")
	disk("sdf", "sdf1")
	disk("sdg")
	disk("sdaa", "sdaa1")
	disk("nvme0n1", "nvme0n1p1")
	disk("nvme0n10")
	disk("loop0")
	disk("dm-0")
	disk("dm-1")
	disk("md0")

	return m
}

func TestScanSysDisks(t *testing.T) {
	resolve := func(dev string) string {
		if dev == "/dev/mapper/vg-data" {
			return "/dev/dm-1"
		}

		return dev
	}

	disks, err := scanSysDisks(testSysFS(), resolve)
	require.NoError(t, err)

	byPath := map[string]DiskInfo{}

	var paths []string

	for _, d := range disks {
		byPath[d.Path] = d
		paths = append(paths, d.Path)
	}

	require.ElementsMatch(t, []string{"/dev/sda", "/dev/sdb", "/dev/sdc", "/dev/sdd", "/dev/sde", "/dev/sdf", "/dev/sdg", "/dev/sdaa", "/dev/nvme0n1", "/dev/nvme0n10", "/dev/md0"}, paths)

	cases := map[string][]DiskVolume{
		"/dev/sda":      {{FileSystem: "/dev/sda2", MountPoints: []string{"/"}}, {FileSystem: "/dev/sda1", MountPoints: []string{"/boot/efi"}}},
		"/dev/sdb":      {{FileSystem: "/dev/sdb1", MountPoints: []string{"/mnt/repo"}}},
		"/dev/sdc":      {{FileSystem: "/dev/sdc (btrfs, with /dev/sdb1)", MountPoints: []string{"/mnt/repo"}}},
		"/dev/sdd":      {{FileSystem: "/dev/mapper/vg-data", MountPoints: []string{"/data"}}},
		"/dev/sde":      {{FileSystem: "/dev/sde1 (swap)"}},
		"/dev/sdf":      {{FileSystem: "used by /dev/md0"}},
		"/dev/sdg":      nil,
		"/dev/sdaa":     {{FileSystem: "/dev/sdaa1", MountPoints: []string{"/mnt/my disk"}}},
		"/dev/nvme0n1":  {{FileSystem: "/dev/nvme0n1p1", MountPoints: []string{"/home"}}},
		"/dev/nvme0n10": nil,
	}

	for path, want := range cases {
		t.Run(path, func(t *testing.T) {
			d := byPath[path]
			require.Equal(t, want, d.Volumes)
			require.Equal(t, want != nil, d.InUse)
			require.Equal(t, int64(1<<30), d.Size)
			require.Equal(t, int64(512), d.SectorSize)
		})
	}

	// every disk of the btrfs repository holds it
	for _, path := range []string{"/dev/sdb", "/dev/sdc"} {
		d := byPath[path]
		require.Equal(t, "holds /mnt/repo/kopia", d.TargetProblem(1<<20, 512, "/mnt/repo/kopia", "/"), path)
	}

	d := byPath["/dev/sdg"]
	require.Empty(t, d.TargetProblem(1<<20, 512, "/mnt/repo/kopia", "/"))

	d = byPath["/dev/sde"]
	require.Equal(t, "in use: /dev/sde1 (swap)", d.TargetProblem(1<<20, 512, "/mnt/repo/kopia", "/"))
}

func TestUnescapeMountField(t *testing.T) {
	require.Equal(t, "/mnt/a b\tc\\d", unescapeMountField(`/mnt/a\040b\011c\134d`))
	require.Equal(t, "/plain", unescapeMountField("/plain"))
	require.Equal(t, `/trailing\04`, unescapeMountField(`/trailing\04`))
}

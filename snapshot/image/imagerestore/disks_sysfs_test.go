package imagerestore

import (
	"fmt"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/require"
)

// testSysFS is a computer with:
//
//   - sda: the system disk, with the ESP and the root file system
//   - sdb and sdc: a btrfs RAID1 holding the repository, mounted from sdb1,
//     with sdc a member as a whole disk; btrfs mounts have anonymous device
//     numbers
//   - sdd: LVM on LUKS on sdd1, mounted from /dev/mapper/vg-data
//   - sde: swap
//   - sdf: a member of an unmounted RAID array
//   - sdg: blank
//   - sdh and sdi: a partitioned RAID array, md1, mounted from md1p1
//   - sdaa: a disk whose name starts with sda, mounted at a path with a space
//   - nvme0n1 with a mounted partition, and the blank nvme0n10
//   - mmcblk0, mounted as /dev/root, which names no device node
//   - loop0, which is not a disk
func testSysFS() fstest.MapFS {
	m := fstest.MapFS{
		"proc/self/mountinfo": {Data: []byte(`21 26 0:20 / /sys rw,nosuid shared:7 - sysfs none rw
26 1 8:2 / / rw,relatime shared:1 - ext4 /dev/sda2 rw
27 26 8:1 / /boot/efi rw,relatime shared:2 - vfat /dev/sda1 rw
28 26 0:45 / /mnt/repo rw,relatime shared:3 - btrfs /dev/sdb1 rw,space_cache=v2
29 26 253:1 / /data rw,relatime shared:4 - ext4 /dev/mapper/vg-data rw
30 26 65:161 / /mnt/my\040disk rw,relatime shared:5 - ext4 /dev/sdaa1 rw
31 26 259:1 / /home rw,relatime shared:6 master:1 - xfs /dev/nvme0n1p1 rw
32 26 179:2 / /srv rw,relatime - ext4 /dev/root rw
33 26 259:9 / /raid rw,relatime - ext4 /dev/md1p1 rw
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
		"sys/block/sdh/holders/md1":       {},
		"sys/block/sdi/holders/md1":       {},
	}

	minor := 0

	disk := func(name string, parts ...string) {
		minor++
		m["sys/block/"+name+"/size"] = &fstest.MapFile{Data: []byte("2097152\n")}
		m["sys/block/"+name+"/queue/logical_block_size"] = &fstest.MapFile{Data: []byte("512\n")}
		m["sys/block/"+name+"/dev"] = &fstest.MapFile{Data: []byte(fmt.Sprintf("100:%v\n", minor))}

		for _, p := range parts {
			minor++
			m["sys/block/"+name+"/"+p+"/partition"] = &fstest.MapFile{Data: []byte("1\n")}
			m["sys/block/"+name+"/"+p+"/dev"] = &fstest.MapFile{Data: []byte(fmt.Sprintf("100:%v\n", minor))}
		}
	}

	disk("sda", "sda1", "sda2")
	disk("sdb", "sdb1")
	disk("sdc")
	disk("sdd", "sdd1")
	disk("sde", "sde1")
	disk("sdf", "sdf1")
	disk("sdg")
	disk("sdh")
	disk("sdi")
	disk("sdaa", "sdaa1")
	disk("nvme0n1", "nvme0n1p1")
	disk("nvme0n10")
	disk("mmcblk0", "mmcblk0p1", "mmcblk0p2")
	disk("loop0")
	disk("dm-0")
	disk("dm-1")
	disk("md0")
	disk("md1", "md1p1")

	// the device numbers that the mounts by device number refer to
	m["sys/block/mmcblk0/mmcblk0p2/dev"] = &fstest.MapFile{Data: []byte("179:2\n")}

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

	require.ElementsMatch(t, []string{"/dev/sda", "/dev/sdb", "/dev/sdc", "/dev/sdd", "/dev/sde", "/dev/sdf", "/dev/sdg", "/dev/sdh", "/dev/sdi", "/dev/sdaa", "/dev/nvme0n1", "/dev/nvme0n10", "/dev/mmcblk0", "/dev/md0", "/dev/md1"}, paths)

	cases := map[string][]DiskVolume{
		"/dev/sda":      {{FileSystem: "/dev/sda2", MountPoints: []string{"/"}}, {FileSystem: "/dev/sda1", MountPoints: []string{"/boot/efi"}}},
		"/dev/sdb":      {{FileSystem: "/dev/sdb1", MountPoints: []string{"/mnt/repo"}}},
		"/dev/sdc":      {{FileSystem: "/dev/sdc (btrfs, with /dev/sdb1)", MountPoints: []string{"/mnt/repo"}}},
		"/dev/sdd":      {{FileSystem: "/dev/mapper/vg-data", MountPoints: []string{"/data"}}},
		"/dev/sde":      {{FileSystem: "/dev/sde1 (swap)"}},
		"/dev/sdf":      {{FileSystem: "used by /dev/md0"}},
		"/dev/sdg":      nil,
		"/dev/sdh":      {{FileSystem: "/dev/md1p1", MountPoints: []string{"/raid"}}},
		"/dev/sdi":      {{FileSystem: "/dev/md1p1", MountPoints: []string{"/raid"}}},
		"/dev/mmcblk0":  {{FileSystem: "/dev/root", MountPoints: []string{"/srv"}}},
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

func TestScanSysDisks_failsWithoutMounts(t *testing.T) {
	fsys := testSysFS()
	delete(fsys, "proc/self/mountinfo")

	_, err := scanSysDisks(fsys, func(dev string) string { return dev })
	require.ErrorContains(t, err, "unable to read the mounts")

	// a kernel without swap has no /proc/swaps
	fsys = testSysFS()
	delete(fsys, "proc/swaps")

	_, err = scanSysDisks(fsys, func(dev string) string { return dev })
	require.NoError(t, err)
}

func TestDiskInfo_differences(t *testing.T) {
	listed := DiskInfo{
		Path: "/dev/sdb", Size: 1 << 30, SectorSize: 512, Model: "SanDisk Ultra", Bus: "USB", Serial: "4C53",
		DiskGUID: "0f6ab3f2-1c3d-4e5f-8a9b-0c1d2e3f4a5b",
		Volumes:  []DiskVolume{{FileSystem: "/dev/sdb1", MountPoints: []string{"/media/usb"}}},
	}

	same := listed
	require.Empty(t, listed.differences(&same))

	cases := map[string]func(d *DiskInfo){
		"size":        func(d *DiskInfo) { d.Size *= 2 },
		"sector size": func(d *DiskInfo) { d.SectorSize = 4096 },
		"model":       func(d *DiskInfo) { d.Model = "Samsung T7" },
		"serial":      func(d *DiskInfo) { d.Serial = "S6WR" },
		"disk GUID":   func(d *DiskInfo) { d.DiskGUID = "" },
		"volumes": func(d *DiskInfo) {
			d.Volumes = []DiskVolume{{FileSystem: "/dev/sdb1", MountPoints: []string{"/mnt/repo"}}}
		},
	}

	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			now := listed
			change(&now)
			require.NotEmpty(t, listed.differences(&now))
		})
	}
}

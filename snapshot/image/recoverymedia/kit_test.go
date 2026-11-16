package recoverymedia

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/snapshot/image"
)

func TestConnectCommand(t *testing.T) {
	cases := []struct {
		name string
		lc   *repo.LocalConfig
		want string
	}{
		{"server pinned", &repo.LocalConfig{APIServer: &repo.APIServerInfo{BaseURL: "https://nas:51515", TrustedServerCertificateFingerprint: "abcd"}}, "kopia repository connect server --url https://nas:51515 --server-cert-fingerprint abcd --readonly"},
		{"server with CA", &repo.LocalConfig{APIServer: &repo.APIServerInfo{BaseURL: "https://nas:51515", RootCA: []byte("x")}}, "kopia repository connect server --url https://nas:51515 --server-ca-pem-path <CA FILE> --readonly"},
		{"folder with a space", storageConfigOf("filesystem", map[string]any{"path": `D:\My Backups`}), `kopia repository connect filesystem --path "D:\\My Backups" --readonly`},
		{
			"s3 without secrets",
			storageConfigOf("s3", map[string]any{"bucket": "b", "endpoint": "s3.lan", "accessKeyID": "AKIA", "secretAccessKey": "s3cr3t"}),
			"kopia repository connect s3 --bucket b --endpoint s3.lan --access-key <ACCESS KEY> --secret-access-key <SECRET KEY> --readonly",
		},
		{"sftp port", storageConfigOf("sftp", map[string]any{"host": "h", "port": float64(2222), "username": "u", "path": "/r"}), "kopia repository connect sftp --host h --port 2222 --username u --path /r --keyfile <KEY FILE> --readonly"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, ConnectCommand(tc.lc))
		})
	}
}

func TestRenderKit(t *testing.T) {
	kit := RenderKit(&Kit{
		Computer:     "DESKTOP",
		BuiltAt:      time.Date(2026, 11, 16, 15, 0, 0, 0, time.UTC),
		KopiaVersion: "0.23.1",
		Connection:   storageConfigOf("s3", map[string]any{"bucket": "backups", "endpoint": "s3.lan", "secretAccessKey": "s3cr3t"}),
		UserHost:     "tim@desktop",
		Disks: []KitDisk{{
			Source:      "tim@desktop:image:disk:0f6ab3f2-1c3d-4e5f-8a9b-0c1d2e3f4a5b",
			LatestImage: "k1234 of 2026-11-16 02:00",
			Layout: &image.Layout{DiskGUID: "0f6ab3f2-1c3d-4e5f-8a9b-0c1d2e3f4a5b", SectorSize: 512, DiskSize: 1 << 40, Partitions: []image.Partition{
				{Number: 1, TypeGUID: image.PartitionTypeEFISystem, Length: 100 << 20},
				{Number: 3, TypeGUID: image.PartitionTypeBasicData, Length: 900 << 30, Name: "Basic data partition"},
			}},
		}},
	})

	require.Contains(t, kit, "KOPIA RECOVERY KIT for DESKTOP\r\n")
	require.Contains(t, kit, "S3 bucket backups at s3.lan")
	require.Contains(t, kit, "partition 1: 104.9 MB, EFI system")
	require.Contains(t, kit, "kopia image restore tim@desktop:image:disk:0f6ab3f2-1c3d-4e5f-8a9b-0c1d2e3f4a5b --target /dev/sdX")
	require.NotContains(t, kit, "s3cr3t")
	require.False(t, strings.Contains(kit, "\n") && !strings.Contains(kit, "\r\n"), "lines end with CRLF for Notepad")
}

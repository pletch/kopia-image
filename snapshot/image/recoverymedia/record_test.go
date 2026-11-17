package recoverymedia

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/internal/testlogging"
	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/snapshot/image"
)

func testLayout(n int64) *image.Layout {
	return &image.Layout{DiskGUID: "0f6ab3f2-1c3d-4e5f-8a9b-0c1d2e3f4a5b", SectorSize: 512, DiskSize: n << 30}
}

func TestRecordStale(t *testing.T) {
	ctx := testlogging.Context(t)
	built := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	conn := storageConfigOf("filesystem", map[string]any{"path": `D:\repo`})

	r := &Record{
		BuiltAt:          built,
		KopiaVersion:     "0.23.1",
		WinREDigest:      "aa",
		ConnectionDigest: ConnectionDigest(conn),
		Disks:            map[string]string{"tim@pc:image:disk:a": LayoutDigest(testLayout(100))},
	}

	fresh := &Current{
		Now:          built.Add(24 * time.Hour),
		KopiaVersion: "0.23.1",
		WinREDigest:  "aa",
		Connection:   storageConfigOf("filesystem", map[string]any{"path": `D:\repo`}),
		Layouts:      map[string]*image.Layout{"tim@pc:image:disk:a": testLayout(100)},
	}
	require.Empty(t, r.Stale(ctx, fresh))

	stale := &Current{
		Now:          built.Add(200 * 24 * time.Hour),
		KopiaVersion: "0.24.0",
		WinREDigest:  "bb",
		Connection:   storageConfigOf("filesystem", map[string]any{"path": `E:\repo`}),
		Layouts: map[string]*image.Layout{
			"tim@pc:image:disk:a": testLayout(200),
			"tim@pc:image:disk:b": testLayout(100),
		},
	}
	require.Equal(t, []string{
		"kopia is now 0.24.0; the media has 0.23.1",
		"Windows updated its recovery environment, which the media is built from; old media may stop booting with Secure Boot on",
		"the repository connection changed",
		"the partitions of tim@pc:image:disk:a changed",
		"the recovery kit does not describe the disk tim@pc:image:disk:b",
		"the media is 200 days old",
	}, r.Stale(ctx, stale))
}

func TestRecordForMedia(t *testing.T) {
	r := &Record{
		BuiltAt:          time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC),
		KopiaVersion:     "0.23.1",
		Target:           `\\.\PhysicalDrive3`,
		WinRE:            `C:\Recovery\WindowsRE\Winre.wim`,
		WinREDigest:      "aa",
		ConnectFrom:      `C:\conn\read-only.config`,
		ConnectionDigest: "cc",
		Disks:            map[string]string{"tim@desktop:image:disk:a": "dd"},
		Repository:       "S3 bucket backups at s3.example.com",
	}
	require.False(t, r.Shared())

	m := r.ForMedia("DESKTOP")
	require.True(t, m.Shared())
	require.Equal(t, "DESKTOP", m.BuiltOn)
	require.Equal(t, "S3 bucket backups at s3.example.com", m.Repository)
	require.Equal(t, r.BuiltAt, m.BuiltAt)

	// the builder's local paths and disks stay off the media
	require.Empty(t, m.Target)
	require.Empty(t, m.WinRE)
	require.Empty(t, m.ConnectFrom)
	require.Nil(t, m.Disks)

	// the original is unchanged
	require.False(t, r.Shared())
	require.Len(t, r.Disks, 1)
}

func TestRecordStale_shared(t *testing.T) {
	ctx := testlogging.Context(t)
	built := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	conn := storageConfigOf("filesystem", map[string]any{"path": `D:\repo`})

	r := &Record{
		BuiltAt:      built,
		KopiaVersion: "0.23.1",
		WinREDigest:  "aa",
		// the builder's connection differs from this computer's...
		ConnectionDigest: "builder",
		BuiltOn:          "DESKTOP",
		// ...but leads to the same repository
		Repository: DescribeRepository(conn),
	}

	fresh := &Current{
		Now:          built.Add(24 * time.Hour),
		KopiaVersion: "0.23.1",
		// this computer's own recovery environment and disks do not matter
		WinREDigest: "bb",
		Connection:  conn,
		Layouts:     map[string]*image.Layout{"tim@laptop:image:disk:z": testLayout(100)},
	}
	require.Empty(t, r.Stale(ctx, fresh))

	stale := &Current{
		Now:          built.Add(200 * 24 * time.Hour),
		KopiaVersion: "0.24.0",
		WinREDigest:  "bb",
		Connection:   storageConfigOf("filesystem", map[string]any{"path": `E:\repo`}),
	}
	require.Equal(t, []string{
		"kopia is now 0.24.0; the media has 0.23.1",
		"the repository connection changed",
		"the media is 200 days old",
	}, r.Stale(ctx, stale))
}

func TestRecordRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), RecordFileName)

	_, err := ReadRecord(p)
	require.ErrorIs(t, err, os.ErrNotExist)

	require.NoError(t, WriteRecord(p, &Record{KopiaVersion: "1", Disks: map[string]string{"s": "d"}}))

	got, err := ReadRecord(p)
	require.NoError(t, err)
	require.Equal(t, "1", got.KopiaVersion)
}

func TestCheckServerTrust(t *testing.T) {
	ctx := testlogging.Context(t)

	srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()

	cert := srv.Certificate()
	sum := sha256.Sum256(cert.Raw)
	pinned := hex.EncodeToString(sum[:])
	caPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}))

	// httptest certificates are for 127.0.0.1 and example.com
	server := func(fp string) *repo.LocalConfig {
		return &repo.LocalConfig{APIServer: &repo.APIServerInfo{BaseURL: srv.URL, TrustedServerCertificateFingerprint: fp}}
	}

	require.NoError(t, CheckServerTrust(ctx, server(pinned), nil))
	require.ErrorContains(t, CheckServerTrust(ctx, server("00"+pinned[2:]), nil), "changed; the media pins the old one")

	require.NoError(t, CheckServerTrust(ctx, server(""), []string{caPEM}))
	require.ErrorContains(t, CheckServerTrust(ctx, server(""), []string{otherCAPEM(t)}), "no longer validate")

	// trusted through the system's authorities: nothing to check
	require.NoError(t, CheckServerTrust(ctx, server(""), nil))
}

func otherCAPEM(t *testing.T) string {
	t.Helper()

	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: selfSignedDER(t)}))
}

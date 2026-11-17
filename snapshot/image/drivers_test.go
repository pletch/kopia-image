package image

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/internal/faketime"
	"github.com/kopia/kopia/internal/repotesting"
	"github.com/kopia/kopia/repo"
)

func writeTestDriver(t *testing.T, files map[string]string) string {
	t.Helper()

	dir := t.TempDir()

	for name, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o700))
		require.NoError(t, os.WriteFile(p, []byte(content), 0o600))
	}

	return dir
}

func TestCreate_storesDrivers(t *testing.T) {
	ctx, env := repotesting.NewEnvironment(t, repotesting.FormatNotImportant)

	net := writeTestDriver(t, map[string]string{"rt640x64.inf": "[Version]", "rt640x64.sys": "driver", "x64/helper.dll": "helper"})
	storage := writeTestDriver(t, map[string]string{"iastorvd.inf": "[Version]"})

	opt := testCreateOptions(t, testVolumeData(), randomBytes(2, 3<<20))
	opt.Drivers = []DriverSource{
		{Class: "Net", INF: "rt640x64.inf", Version: "10.65", Entry: "oem40", Dir: net},
		{Class: "SCSIAdapter", INF: "iastorvd.inf", Version: "19.5", Entry: "oem3", Dir: storage},
		{Class: "USB", INF: "gone.inf", Version: "1.0", Entry: "oem9", Dir: filepath.Join(t.TempDir(), "missing")},
	}

	man, md, err := Create(ctx, env.RepositoryWriter, opt)
	require.NoError(t, err, "a driver that cannot be stored does not fail the image")

	require.Equal(t, []Driver{
		{Class: "Net", INF: "rt640x64.inf", Version: "10.65", Entry: "oem40"},
		{Class: "SCSIAdapter", INF: "iastorvd.inf", Version: "19.5", Entry: "oem3"},
	}, md.Drivers)

	read, err := ReadMetadata(ctx, env.Repository, man)
	require.NoError(t, err)
	require.Equal(t, md.Drivers, read.Drivers)

	dest := filepath.Join(t.TempDir(), "net")
	require.NoError(t, ExtractDriver(ctx, env.Repository, man, md.Drivers[0], dest))

	b, err := os.ReadFile(filepath.Join(dest, "x64", "helper.dll"))
	require.NoError(t, err)
	require.Equal(t, "helper", string(b))

	b, err = os.ReadFile(filepath.Join(dest, "rt640x64.sys"))
	require.NoError(t, err)
	require.Equal(t, "driver", string(b))

	require.Error(t, ExtractDriver(ctx, env.Repository, man, Driver{Entry: "oem9"}, filepath.Join(t.TempDir(), "x")))
	require.Error(t, ExtractDriver(ctx, env.Repository, man, Driver{Entry: "../oem3"}, filepath.Join(t.TempDir(), "y")))
}

func TestCreate_withoutDrivers(t *testing.T) {
	ctx, env := repotesting.NewEnvironment(t, repotesting.FormatNotImportant)

	man, md, err := Create(ctx, env.RepositoryWriter, testCreateOptions(t, testVolumeData(), randomBytes(2, 3<<20)))
	require.NoError(t, err)
	require.Empty(t, md.Drivers)
	require.Error(t, ExtractDriver(ctx, env.Repository, man, Driver{Entry: "oem3"}, filepath.Join(t.TempDir(), "x")))
}

func TestSafeEntryName(t *testing.T) {
	require.Equal(t, "oem40", safeEntryName("oem40"))
	require.Equal(t, "a_b.c", safeEntryName("a/b.c"))
	require.Empty(t, safeEntryName(".."))
	require.Empty(t, safeEntryName(""))
}

func TestSameDriver(t *testing.T) {
	require.True(t, SameDriver(Driver{INF: "RT640x64.inf"}, Driver{INF: "rt640x64.inf", Version: "1"}))
	require.False(t, SameDriver(Driver{INF: "a.inf"}, Driver{INF: "b.inf"}))
	require.Equal(t, "Net rt640x64.inf 10.65", Driver{Class: "Net", INF: "rt640x64.inf", Version: "10.65", Entry: "oem40"}.String())
}

func TestDriverInventory(t *testing.T) {
	ft := faketime.NewTimeAdvance(time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC))
	ctx, env := repotesting.NewEnvironment(t, repotesting.FormatNotImportant, repotesting.Options{
		OpenOptions: func(o *repo.Options) { o.TimeNowFunc = ft.NowFunc() },
	})

	dir := writeTestDriver(t, map[string]string{"a.inf": "[Version]"})

	create := func(host, version string) {
		opt := testCreateOptions(t, testVolumeData(), randomBytes(2, 3<<20))
		opt.Source.Host = host

		if version != "" {
			opt.Drivers = []DriverSource{{Class: "Net", INF: "a.inf", Version: version, Entry: "oem1", Dir: dir}}
		}

		_, _, err := Create(ctx, env.RepositoryWriter, opt)
		require.NoError(t, err)
		ft.Advance(time.Minute)
	}

	create("laptop", "1.0")
	create("laptop", "2.0")
	// the newest image of the laptop stores no drivers: the one before does
	create("laptop", "")
	create("desktop", "3.0")
	create("nas", "")

	inv, err := DriverInventory(ctx, env.Repository)
	require.NoError(t, err)
	require.Len(t, inv, 2)
	require.Equal(t, "desktop", inv[0].Host)
	require.Equal(t, "3.0", inv[0].Drivers[0].Version)
	require.Equal(t, "laptop", inv[1].Host)
	require.Equal(t, "2.0", inv[1].Drivers[0].Version)
	require.NotNil(t, inv[1].Image)
}

package ntfsfs

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/internal/mount"
	"github.com/kopia/kopia/internal/vhdxtest"
)

// TestMountedImageShowsFiles mounts an image snapshot the way the Kopia UI's
// Mount button does, as a drive, and reads a file of the NTFS volume through
// it with ordinary file APIs.
func TestMountedImageShowsFiles(t *testing.T) {
	if os.Getenv(vhdxtest.EnvName) == "" {
		t.Skipf("set %v=1 to run tests that mount drives", vhdxtest.EnvName)
	}

	ctx, env, root := createTestImageSnapshot(t)

	ctrl, err := mount.Directory(ctx, WithBrowsableVolumes(ctx, env.Repository, root), "*", mount.Options{DriveLabel: "kopia image test"})
	require.NoError(t, err)

	t.Cleanup(func() { require.NoError(t, ctrl.Unmount(context.Background())) })

	mp := ctrl.MountPath()
	t.Logf("mounted at %v", mp)

	entries, err := os.ReadDir(mp)
	require.NoError(t, err)

	var rootNames []string
	for _, e := range entries {
		rootNames = append(rootNames, e.Name())
	}

	require.Contains(t, rootNames, "p2-C")

	b, err := os.ReadFile(filepath.Join(mp, "p2-C", "Folder A", "Folder B", "Hello world text document.txt"))
	require.NoError(t, err)
	require.Equal(t, "Hello world!", string(b))

	ones, err := os.ReadFile(filepath.Join(mp, "p2-C", "ones.bin"))
	require.NoError(t, err)
	require.Len(t, ones, 2949120)
}

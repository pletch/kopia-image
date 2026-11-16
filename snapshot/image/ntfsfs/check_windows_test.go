package ntfsfs

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mxk/go-vss"
	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/internal/vhdxtest"
	"github.com/kopia/kopia/snapshot/image/blockdev"
)

// liveCheckEnv names a volume, such as C:\, whose shadow copy
// TestCheckLiveVolume checks.
const liveCheckEnv = "KOPIA_IMAGE_NTFS_CHECK_VOLUME"

func TestCheckShadowCopyOfVHDX(t *testing.T) {
	mnt := vhdxtest.New(t).FormatAndMount()

	// fragmented files, deleted files, nested directories, hard links and
	// alternate data streams
	for i := range 40 {
		dir := filepath.Join(mnt, fmt.Sprintf("dir%v", i%5), "sub")
		require.NoError(t, os.MkdirAll(dir, 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%02d.bin", i)), bytes.Repeat([]byte{byte(i)}, (i+1)*50_000), 0o600))
	}

	for i := 0; i < 40; i += 3 {
		require.NoError(t, os.Remove(filepath.Join(mnt, fmt.Sprintf("dir%v", i%5), "sub", fmt.Sprintf("f%02d.bin", i))))
	}

	require.NoError(t, os.WriteFile(filepath.Join(mnt, "big.bin"), bytes.Repeat([]byte("kopia"), 4<<20), 0o600))
	require.NoError(t, os.Link(filepath.Join(mnt, "big.bin"), filepath.Join(mnt, "dir1", "link.bin")))
	require.NoError(t, os.WriteFile(filepath.Join(mnt, "big.bin:stream"), []byte("ads"), 0o600))
	require.NoError(t, os.RemoveAll(filepath.Join(mnt, "dir3")))

	r := checkShadowCopy(t, vhdxtest.VolumeGUIDPath(t, mnt))

	require.True(t, r.OK(), "%v", r.Problems)
	require.GreaterOrEqual(t, r.Files, int64(27))
	require.GreaterOrEqual(t, r.Directories, int64(9))
}

// TestCheckLiveVolume checks a shadow copy of a real volume, to find false
// positives on file systems with years of history.
func TestCheckLiveVolume(t *testing.T) {
	volume := os.Getenv(liveCheckEnv)
	if volume == "" {
		t.Skipf("set %v to a volume such as C:\\ to check a shadow copy of it (requires administrator)", liveCheckEnv)
	}

	r := checkShadowCopy(t, volume)

	require.True(t, r.OK(), "%v problems: %v", r.ProblemCount, r.Problems)
}

func checkShadowCopy(t *testing.T, volume string) *CheckResult {
	t.Helper()

	var id string

	for attempt := 1; ; attempt++ {
		var err error

		if id, err = vss.Create(volume); err == nil {
			break
		}

		var e vss.CreateError
		if !errors.As(err, &e) || e != 9 || attempt == 10 {
			require.NoError(t, err)
		}

		time.Sleep(5 * time.Second) //nolint:forbidigo
	}

	t.Cleanup(func() { require.NoError(t, vss.Remove(id)) })

	sc, err := vss.Get(id)
	require.NoError(t, err)

	dev, err := blockdev.Open(sc.DeviceObject)
	require.NoError(t, err)

	t.Cleanup(func() { dev.Close() }) //nolint:errcheck

	start := time.Now() //nolint:forbidigo

	r, err := CheckDevice(t.Context(), dev, dev.Size(), CheckOptions{})
	require.NoError(t, err)

	t.Logf("checked %v in %v: %+v", volume, time.Since(start), *r) //nolint:forbidigo

	return r
}

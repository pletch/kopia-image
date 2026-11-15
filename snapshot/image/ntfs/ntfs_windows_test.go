package ntfs_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/mxk/go-vss"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"

	"github.com/kopia/kopia/snapshot/image"
	"github.com/kopia/kopia/snapshot/image/blockdev"
	"github.com/kopia/kopia/snapshot/image/ntfs"
)

// vhdxTestEnv enables tests that attach a VHDX and create shadow copies.
// They need administrator rights.
const vhdxTestEnv = "KOPIA_IMAGE_VHDX_TEST"

// diskpart runs a diskpart script. Paths in scripts are quoted with
// diskpartQuote; Go's %q would double their backslashes.
func diskpart(ctx context.Context, dir, script string) ([]byte, error) {
	f, err := os.CreateTemp(dir, "diskpart-*.txt")
	if err != nil {
		return nil, err
	}

	_, writeErr := f.WriteString(script)
	if err := errors.Join(writeErr, f.Close()); err != nil {
		return nil, err
	}

	return exec.CommandContext(ctx, "diskpart", "/s", f.Name()).CombinedOutput()
}

func diskpartQuote(path string) string {
	return `"` + path + `"`
}

func runDiskpart(t *testing.T, dir, script string) {
	t.Helper()

	out, err := diskpart(t.Context(), dir, script)
	require.NoError(t, err, "diskpart failed:\n%s", out)
}

// attachTestVolume creates, attaches and mounts a 1 GiB GPT disk with one
// NTFS volume and returns the mount directory.
func attachTestVolume(t *testing.T) string {
	t.Helper()

	if os.Getenv(vhdxTestEnv) == "" {
		t.Skipf("set %v=1 to run tests that attach a VHDX (requires administrator)", vhdxTestEnv)
	}

	dir := t.TempDir()
	vhdx := filepath.Join(dir, "test.vhdx")
	mnt := filepath.Join(dir, "mnt")
	require.NoError(t, os.Mkdir(mnt, 0o700))

	runDiskpart(t, dir, "create vdisk file="+diskpartQuote(vhdx)+" maximum=1024 type=expandable\n")

	t.Cleanup(func() {
		// t.Context() is already canceled when cleanups run
		out, err := diskpart(context.Background(), dir, "select vdisk file="+diskpartQuote(vhdx)+"\ndetach vdisk\n")
		if err != nil {
			t.Logf("unable to detach %v: %v\n%s", vhdx, err, out)
		}
	})

	runDiskpart(t, dir, "select vdisk file="+diskpartQuote(vhdx)+`
attach vdisk
convert gpt
create partition primary
format fs=ntfs quick label=kopiaimg
assign mount=`+diskpartQuote(mnt)+"\n")

	return mnt
}

func volumeGUIDPath(t *testing.T, mnt string) string {
	t.Helper()

	buf := make([]uint16, windows.MAX_PATH)
	require.NoError(t, windows.GetVolumeNameForVolumeMountPoint(windows.StringToUTF16Ptr(mnt+`\`), &buf[0], uint32(len(buf))))

	return windows.UTF16ToString(buf)
}

func readExtents(t *testing.T, r io.ReaderAt, extents []image.Extent, n int) []byte {
	t.Helper()

	var out []byte

	for _, e := range extents {
		b := make([]byte, e.Length)
		_, err := r.ReadAt(b, e.Start)
		require.NoError(t, err)

		out = append(out, b...)
	}

	require.GreaterOrEqual(t, len(out), n)

	return out[:n]
}

func isAllocated(bitmap []byte, cluster int64) bool {
	return bitmap[cluster/8]&(1<<(cluster%8)) != 0
}

func TestProbeShadowCopyOfVHDX(t *testing.T) {
	mnt := attachTestVolume(t)

	// a fragmented free-space pattern and a file whose clusters we can check
	for i := range 20 {
		require.NoError(t, os.WriteFile(filepath.Join(mnt, fmt.Sprintf("filler%02d.bin", i)), bytes.Repeat([]byte{byte(i + 1)}, 1<<20), 0o600))
	}

	for i := 0; i < 20; i += 2 {
		require.NoError(t, os.Remove(filepath.Join(mnt, fmt.Sprintf("filler%02d.bin", i))))
	}

	content := make([]byte, 20<<20+1234)
	_, err := rand.Read(content)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(mnt, "big.bin"), content, 0o600))

	volume := volumeGUIDPath(t, mnt)

	extents, err := blockdev.VolumeDiskExtents(volume)
	require.NoError(t, err)
	require.Len(t, extents, 1)

	layout, err := blockdev.ReadLayout(extents[0].DiskNumber)
	require.NoError(t, err)
	t.Logf("layout: %+v", layout)

	var part *image.Partition

	for i := range layout.Partitions {
		if layout.Partitions[i].Offset == extents[0].Offset {
			part = &layout.Partitions[i]
		}
	}

	require.NotNil(t, part, "no partition at volume offset %v", extents[0].Offset)
	require.Equal(t, image.PartitionTypeBasicData, part.TypeGUID)
	require.Equal(t, extents[0].Length, part.Length)

	id, err := vss.Create(volume)
	require.NoError(t, err)

	t.Cleanup(func() { require.NoError(t, vss.Remove(id)) })

	sc, err := vss.Get(id)
	require.NoError(t, err)

	// changes after the shadow copy must not be visible in it
	require.NoError(t, os.WriteFile(filepath.Join(mnt, "big.bin"), make([]byte, len(content)), 0o600))

	require.NoError(t, ntfs.EnableBackupPrivilege())

	dev, err := blockdev.Open(sc.DeviceObject)
	require.NoError(t, err)

	defer dev.Close()

	require.Equal(t, part.Length, dev.Size())

	vol, err := ntfs.Probe(dev)
	require.NoError(t, err)
	t.Logf("volume data: %+v, unfrozen files: %v", vol.Data, vol.UnfrozenFiles)

	require.Equal(t, int64(4096), vol.Data.ClusterSize)

	free, err := vol.FreeExtents()
	require.NoError(t, err)
	require.Equal(t, vol.Data.FreeClusters*vol.Data.ClusterSize, image.TotalLength(free))

	for _, e := range vol.UnfrozenExtents {
		for c := e.Start / vol.Data.ClusterSize; c < e.End()/vol.Data.ClusterSize; c++ {
			require.True(t, isAllocated(vol.Bitmap, c), "unfrozen cluster %v is free", c)
		}
	}

	// the boot sector, read unaligned through the bounce buffer, and its
	// backup in the last sector beyond the last whole cluster
	boot := make([]byte, 512)
	_, err = dev.ReadAt(boot, 0)
	require.NoError(t, err)
	require.Equal(t, "NTFS    ", string(boot[3:11]))

	backup := make([]byte, 512)
	_, err = dev.ReadAt(backup, dev.Size()-512)
	require.NoError(t, err)
	require.Equal(t, boot, backup)

	bigExtents, err := ntfs.FileExtents(sc.DeviceObject+`\big.bin`, vol.Data.ClusterSize)
	require.NoError(t, err)
	require.Equal(t, content, readExtents(t, dev, bigExtents, len(content)), "shadow copy must show the original content")

	mask, err := vol.ZeroMask()
	require.NoError(t, err)

	mr, err := image.NewMaskedReader(dev, dev.Size(), mask)
	require.NoError(t, err)

	imageCopy := make([]byte, dev.Size())
	buf := blockdev.AlignedBuffer(image.DefaultChunkSize)

	for off := int64(0); off < dev.Size(); {
		n, err := mr.ReadAt(buf, off)
		copy(imageCopy[off:], buf[:n])
		off += int64(n)

		if err != nil {
			require.ErrorIs(t, err, io.EOF)
			break
		}
	}

	require.Equal(t, dev.Size()-mr.MaskedBytes(), mr.SourceBytesRead())
	require.Equal(t, content, readExtents(t, bytes.NewReader(imageCopy), bigExtents, len(content)))
	require.Equal(t, boot, imageCopy[len(imageCopy)-512:])

	for _, e := range free {
		require.Equal(t, make([]byte, e.Length), imageCopy[e.Start:e.End()], "free extent %+v not zero", e)
	}
}

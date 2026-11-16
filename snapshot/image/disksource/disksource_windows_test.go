package disksource_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"

	"github.com/kopia/kopia/internal/repotesting"
	"github.com/kopia/kopia/internal/vhdxtest"
	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/object"
	"github.com/kopia/kopia/snapshot"
	"github.com/kopia/kopia/snapshot/image"
	"github.com/kopia/kopia/snapshot/image/blockdev"
	"github.com/kopia/kopia/snapshot/image/disksource"
)

// populate writes a mix of large, small (MFT-resident), nested, deleted and
// alternate-stream files and returns the expected contents by relative path.
func populate(t *testing.T, root string) map[string][]byte {
	t.Helper()

	want := map[string][]byte{}

	put := func(rel string, data []byte) {
		p := filepath.Join(root, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o700))
		require.NoError(t, os.WriteFile(p, data, 0o600))

		want[rel] = data
	}

	big := make([]byte, 30<<20+777)
	_, err := rand.Read(big)
	require.NoError(t, err)
	put("big.bin", big)

	for i := range 300 {
		put(filepath.Join(fmt.Sprintf("dir%02d", i%17), fmt.Sprintf("sub%d", i%3), fmt.Sprintf("file%03d.txt", i)),
			bytes.Repeat([]byte(fmt.Sprintf("file %v ", i)), i*7))
	}

	// deletions rewrite directory index blocks outside $MFT
	for rel := range want {
		if rel != "" && rel[len(rel)-5] == '3' {
			require.NoError(t, os.Remove(filepath.Join(root, rel)))
			delete(want, rel)
		}
	}

	put("ads.txt", []byte("main stream"))
	put("ads.txt:extra", []byte("alternate data stream"))

	return want
}

// verifyFiles checks that root holds exactly the expected file contents.
func verifyFiles(t *testing.T, root string, want map[string][]byte) {
	t.Helper()

	for rel, data := range want {
		got, err := os.ReadFile(filepath.Join(root, rel))
		require.NoError(t, err, rel)
		require.True(t, bytes.Equal(data, got), "contents of %v differ", rel)
	}

	var count int

	// the trailing separator makes WalkDir enter the volume mount point
	require.NoError(t, filepath.WalkDir(root+`\`, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if d.IsDir() && d.Name() == "System Volume Information" {
			return filepath.SkipDir
		}

		if !d.IsDir() {
			count++
		}

		return nil
	}))

	// the alternate stream is not listed as a file
	require.Equal(t, len(want)-1, count, "unexpected files in the restored volume")
}

// writeImageToDisk writes an image object to a disk at an offset, as
// "kopia image restore" will.
func writeImageToDisk(ctx context.Context, t *testing.T, rep repo.Repository, oid object.ID, diskNumber uint32, offset int64) {
	t.Helper()

	path := blockdev.DiskPath(diskNumber)

	h, err := windows.CreateFile(windows.StringToUTF16Ptr(path),
		windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_NO_BUFFERING|windows.FILE_FLAG_WRITE_THROUGH, 0)
	require.NoError(t, err)

	f := os.NewFile(uintptr(h), path)
	defer f.Close()

	r, err := rep.OpenObject(ctx, oid)
	require.NoError(t, err)

	defer r.Close()

	buf := blockdev.AlignedBuffer(image.DefaultChunkSize)
	zero := make([]byte, image.DefaultChunkSize)

	for off := int64(0); ; {
		n, err := io.ReadFull(r, buf)

		// a new VHDX reads as zeros, so zero chunks need not be written
		if n > 0 && !bytes.Equal(buf[:n], zero[:n]) {
			_, werr := f.WriteAt(buf[:n], offset+off)
			require.NoError(t, werr)
		}

		off += int64(n)

		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return
		}

		require.NoError(t, err)
	}
}

func TestImageAndRestoreVHDX(t *testing.T) {
	src := vhdxtest.New(t)
	mnt := src.FormatAndMount()
	want := populate(t, mnt)
	volume := vhdxtest.VolumeGUIDPath(t, mnt)

	disks, err := disksource.DisksForVolumes([]string{volume})
	require.NoError(t, err)
	require.Equal(t, []uint32{src.Number()}, disks)

	plan, err := disksource.PrepareDisk(disks[0])
	require.NoError(t, err)

	var sb bytes.Buffer

	plan.Print(&sb)
	t.Logf("plan:\n%s", sb.String())

	require.Len(t, plan.Partitions, 1)
	require.Equal(t, image.VolumeKindNTFS, plan.Partitions[0].Kind)
	require.Equal(t, vhdxtest.DataPartition, plan.Partitions[0].Partition.Number)
	require.Len(t, plan.Skipped, 1, "the MSR is not imaged")

	statePath := filepath.Join(t.TempDir(), "state.json")

	guard, err := disksource.Acquire(statePath, t.Logf)
	require.NoError(t, err)

	session, err := disksource.OpenSession(t.Context(), plan, guard.State, t.Logf)
	require.NoError(t, err)
	require.Len(t, guard.State.IDs(), 1, "the shadow copy is recorded")

	// changes after the shadow copy must not reach the image
	require.NoError(t, os.WriteFile(filepath.Join(mnt, "big.bin"), []byte("changed"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(mnt, "late.txt"), []byte("late"), 0o600))

	ctx, env := repotesting.NewEnvironment(t, repotesting.FormatNotImportant)

	sourcePath, err := image.DiskSourcePath(plan.Layout.DiskGUID)
	require.NoError(t, err)

	man, md, err := image.Create(ctx, env.RepositoryWriter, &image.CreateOptions{
		Source:             snapshot.SourceInfo{Host: "test", UserName: "test", Path: sourcePath},
		Layout:             plan.Layout,
		Volumes:            session.Volumes(),
		Compressor:         "zstd-fastest",
		MetadataCompressor: "zstd-fastest",
		AsyncWrites:        4,
		FlushInterval:      time.Second,
		NewBuffer:          blockdev.AlignedBuffer,
		Logf:               t.Logf,
	})
	require.NoError(t, err)

	require.NoError(t, session.Close())
	require.Empty(t, guard.State.IDs(), "the shadow copy is removed")
	require.NoError(t, guard.Close())

	t.Logf("snapshot %v: %+v, uploaded %v", man.ID, md.Volumes[0].Stats, *md.UploadedBytes)
	require.NotEmpty(t, md.Volumes[0].UnstableExtents, "the diff area file is zeroed")

	// restore to a blank disk with the same layout and check the result
	dst := vhdxtest.New(t)

	dstLayout, err := blockdev.ReadLayout(dst.Number())
	require.NoError(t, err)

	part, ok := dstLayout.Partition(vhdxtest.DataPartition)
	require.True(t, ok)
	require.Equal(t, plan.Partitions[0].Partition.Offset, part.Offset)
	require.Equal(t, plan.Partitions[0].Partition.Length, part.Length)

	writeImageToDisk(ctx, t, env.Repository, md.Volumes[0].ObjectID, dst.Number(), part.Offset)

	dst.Reattach()
	restored := dst.Mount()

	verifyFiles(t, restored, want)

	out, err := exec.CommandContext(t.Context(), "chkdsk", restored, "/scan").CombinedOutput()
	t.Logf("chkdsk:\n%s", out)
	require.NoError(t, err, "chkdsk found problems in the restored volume")
}

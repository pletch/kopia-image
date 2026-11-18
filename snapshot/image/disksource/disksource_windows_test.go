package disksource_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	kopiafs "github.com/kopia/kopia/fs"
	"github.com/kopia/kopia/internal/repotesting"
	"github.com/kopia/kopia/internal/vhdxtest"
	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/object"
	"github.com/kopia/kopia/snapshot"
	"github.com/kopia/kopia/snapshot/image"
	"github.com/kopia/kopia/snapshot/image/blockdev"
	"github.com/kopia/kopia/snapshot/image/disksource"
	"github.com/kopia/kopia/snapshot/image/imagerestore"
	"github.com/kopia/kopia/snapshot/image/ntfsfs"
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

// writeImageToDisk writes an image object into a partition of a disk with
// kopia image restore's own target, which locks the disk's volumes so that
// Windows cannot mount the partition while it is written.
func writeImageToDisk(ctx context.Context, t *testing.T, rep repo.Repository, oid object.ID, diskNumber uint32, offset int64) {
	t.Helper()

	target, err := imagerestore.OpenTarget(blockdev.DiskPath(diskNumber), 0, 0)
	require.NoError(t, err)

	// a new VHDX reads as zeros, so zero chunks need not be written
	_, err = imagerestore.WriteObject(ctx, rep, oid, target, offset, imagerestore.Options{TargetIsZeroed: true})
	require.NoError(t, err)

	require.NoError(t, target.Sync())
	require.NoError(t, target.Close())
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

	session, err := disksource.OpenSession(t.Context(), plan, guard.State, disksource.SessionOptions{}, t.Logf)
	require.NoError(t, err)
	require.Len(t, guard.State.Records(), 1, "the shadow copy is recorded")

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
	require.Empty(t, guard.State.Records(), "the shadow copy is removed")
	require.NoError(t, guard.Close())

	t.Logf("snapshot %v: %+v, uploaded %v", man.ID, md.Volumes[0].Stats, *md.UploadedBytes)
	require.NotEmpty(t, md.Volumes[0].UnstableExtents, "the diff area file is zeroed")

	verifyBrowsedFiles(ctx, t, env.Repository, &md.Volumes[0], want)

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

// verifyBrowsedFiles reads every expected file, including the alternate data
// stream, from the stored image through ntfsfs, without restoring it.
func verifyBrowsedFiles(ctx context.Context, t *testing.T, rep repo.Repository, v *image.Volume, want map[string][]byte) {
	t.Helper()

	vol, err := ntfsfs.Open(ctx, rep, v.ObjectID, "p2-data", ntfsfs.Options{})
	require.NoError(t, err)

	defer vol.Close()

	for rel, data := range want {
		var e kopiafs.Entry = vol.Root()

		for part := range strings.SplitSeq(filepath.ToSlash(rel), "/") {
			d, ok := e.(kopiafs.Directory)
			require.True(t, ok, rel)

			e, err = d.Child(ctx, part)
			require.NoError(t, err, rel)
		}

		f, ok := e.(kopiafs.File)
		require.True(t, ok, rel)

		r, err := f.Open(ctx)
		require.NoError(t, err, rel)

		got, err := io.ReadAll(r)
		require.NoError(t, err, rel)
		require.NoError(t, r.Close())
		require.True(t, bytes.Equal(data, got), "contents of %v differ when browsed", rel)
	}

	chunks := (v.Size + image.DefaultChunkSize - 1) / image.DefaultChunkSize
	t.Logf("browsing every file fetched %v of %v image chunks", vol.Fetches(), chunks)
	require.Less(t, vol.Fetches(), chunks)
}

// TestExcludeKopiaCachesVHDX images a volume holding a kopia cache: the
// cache's files are restored as zeros, except the committed index cache, and
// everything else is restored as it was.
func TestExcludeKopiaCachesVHDX(t *testing.T) {
	src := vhdxtest.New(t)
	mnt := src.FormatAndMount()

	cache := filepath.Join(mnt, "Users", "someone", "AppData", "Local", "kopia", "0123456789abcdef")
	require.NoError(t, os.MkdirAll(filepath.Join(cache, "server-contents", "ab"), 0o700))
	require.NoError(t, os.MkdirAll(filepath.Join(cache, "indexes"), 0o700))

	random := func(n int) []byte {
		b := make([]byte, n)
		_, err := rand.Read(b)
		require.NoError(t, err)

		return b
	}

	cached := random(3 << 20)
	index := random(64 << 10)
	other := random(1 << 20)

	require.NoError(t, os.WriteFile(filepath.Join(cache, "server-contents", "ab", "abcdef.f"), cached, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(cache, "indexes", "x1.sndx"), index, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(mnt, "other.bin"), other, 0o600))

	disks, err := disksource.DisksForVolumes([]string{vhdxtest.VolumeGUIDPath(t, mnt)})
	require.NoError(t, err)

	plan, err := disksource.PrepareDisk(disks[0])
	require.NoError(t, err)

	guard, err := disksource.Acquire(filepath.Join(t.TempDir(), "state.json"), t.Logf)
	require.NoError(t, err)

	session, err := disksource.OpenSession(t.Context(), plan, guard.State, disksource.SessionOptions{ExcludeKopiaCaches: true}, t.Logf)
	require.NoError(t, err)

	ctx, env := repotesting.NewEnvironment(t, repotesting.FormatNotImportant)

	sourcePath, err := image.DiskSourcePath(plan.Layout.DiskGUID)
	require.NoError(t, err)

	_, md, err := image.Create(ctx, env.RepositoryWriter, &image.CreateOptions{
		Source:    snapshot.SourceInfo{Host: "test", UserName: "test", Path: sourcePath},
		Layout:    plan.Layout,
		Volumes:   session.Volumes(),
		NewBuffer: blockdev.AlignedBuffer,
		Logf:      t.Logf,
	})
	require.NoError(t, err)

	require.NoError(t, session.Close())
	require.NoError(t, guard.Close())

	require.Equal(t, []image.ExcludedFolder{{
		Path:  `Users\someone\AppData\Local\kopia\0123456789abcdef\server-contents`,
		Files: 1,
		Bytes: 3 << 20,
	}}, md.Volumes[0].Excluded)

	dst := vhdxtest.New(t)

	dstLayout, err := blockdev.ReadLayout(dst.Number())
	require.NoError(t, err)

	part, ok := dstLayout.Partition(vhdxtest.DataPartition)
	require.True(t, ok)

	writeImageToDisk(ctx, t, env.Repository, md.Volumes[0].ObjectID, dst.Number(), part.Offset)

	dst.Reattach()
	restored := dst.Mount()
	restoredCache := filepath.Join(restored, "Users", "someone", "AppData", "Local", "kopia", "0123456789abcdef")

	read := func(path string) []byte {
		b, err := os.ReadFile(path)
		require.NoError(t, err)

		return b
	}

	require.Equal(t, make([]byte, len(cached)), read(filepath.Join(restoredCache, "server-contents", "ab", "abcdef.f")), "the cache file is restored as zeros")
	require.Equal(t, index, read(filepath.Join(restoredCache, "indexes", "x1.sndx")), "the index cache is imaged")
	require.Equal(t, other, read(filepath.Join(restored, "other.bin")))

	out, err := exec.CommandContext(t.Context(), "chkdsk", restored, "/scan").CombinedOutput()
	t.Logf("chkdsk:\n%s", out)
	require.NoError(t, err, "chkdsk found problems in the restored volume")
}

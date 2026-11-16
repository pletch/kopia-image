package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/internal/repotesting"
	"github.com/kopia/kopia/snapshot"
	"github.com/kopia/kopia/snapshot/image"
)

// createNTFSTestImage stores go-ntfs's NTFS test image as partition 2 of an
// image snapshot, next to a raw ESP, and returns the snapshot ID.
func createNTFSTestImage(ctx context.Context, t *testing.T, env *repotesting.Environment) string {
	t.Helper()

	out, err := exec.CommandContext(ctx, "go", "list", "-m", "-f", "{{.Dir}}", "www.velocidex.com/golang/go-ntfs").Output()
	if err != nil {
		t.Skipf("go-ntfs module not found: %v", err)
	}

	ntfsData, err := os.ReadFile(filepath.Join(strings.TrimSpace(string(out)), "parser", "test_data", "test.ntfs.dd"))
	require.NoError(t, err)

	espData := bytes.Repeat([]byte{1}, 1<<20)

	imagePath, err := image.DiskSourcePath(testImageDiskGUID)
	require.NoError(t, err)

	open := func(b []byte) func() (io.ReadCloser, error) {
		return func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(b)), nil }
	}

	man, _, err := image.Create(ctx, env.RepositoryWriter, &image.CreateOptions{
		Source: snapshot.SourceInfo{
			Host:     env.Repository.ClientOptions().Hostname,
			UserName: env.Repository.ClientOptions().Username,
			Path:     imagePath,
		},
		Layout: &image.Layout{
			DiskGUID:   testImageDiskGUID,
			SectorSize: 512,
			DiskSize:   1 << 30,
			Partitions: []image.Partition{
				{Number: 1, TypeGUID: image.PartitionTypeEFISystem, UniqueGUID: "11111111-2222-4333-8444-555555555551", Offset: 1 << 20, Length: 8 << 20, Name: "EFI system partition"},
				{Number: 2, TypeGUID: image.PartitionTypeBasicData, UniqueGUID: "11111111-2222-4333-8444-555555555552", Offset: 9 << 20, Length: 16 << 20, Name: "Basic data partition"},
			},
		},
		Volumes: []image.VolumeSource{
			{PartitionNumber: 1, Label: "esp", Kind: image.VolumeKindRaw, Size: int64(len(espData)), Open: open(espData)},
			{PartitionNumber: 2, Label: "C", Kind: image.VolumeKindNTFS, Size: int64(len(ntfsData)), Open: open(ntfsData)},
		},
	})
	require.NoError(t, err)

	return string(man.ID)
}

// captureStdout runs f with a textOutput writing to a buffer.
func captureStdout(t *testing.T, f func(out textOutput) error) (string, error) {
	t.Helper()

	r, w, err := os.Pipe()
	require.NoError(t, err)

	orig := os.Stdout
	os.Stdout = w

	runErr := f(textOutput{})

	os.Stdout = orig

	require.NoError(t, w.Close())

	b, err := io.ReadAll(r)
	require.NoError(t, err)

	return string(b), runErr
}

func TestImageLs(t *testing.T) {
	ctx, env := repotesting.NewEnvironment(t, repotesting.FormatNotImportant)
	id := createNTFSTestImage(ctx, t, env)

	run := func(path string, long bool) (string, error) {
		return captureStdout(t, func(out textOutput) error {
			return (&commandImageLs{path: path, long: long, out: out}).run(ctx, env.Repository)
		})
	}

	// the snapshot's volumes
	out, err := run(id, false)
	require.NoError(t, err)
	require.Contains(t, out, "p1-esp")
	require.Contains(t, out, "p2-C")

	// a folder, by entry name, name without .img, and partition number, with either separator
	for _, p := range []string{id + "/p2-C.img/Folder A/Folder B", id + "/p2-C/Folder A/Folder B", id + `/2\Folder A\Folder B`} {
		out, err = run(p, false)
		require.NoError(t, err, p)
		require.Equal(t, "Hello world text document.txt\nHello world text document.txt:goodbye.txt\n", out, p)
	}

	out, err = run(id+"/p2-C", true)
	require.NoError(t, err)
	require.Contains(t, out, "Folder A/")
	require.Contains(t, out, "ones.bin")
	require.Contains(t, out, "2.9 MB")

	// a single file
	out, err = run(id+"/p2-C/ones.bin", false)
	require.NoError(t, err)
	require.Equal(t, "ones.bin\n", out)

	_, err = run(id+"/p1-esp", false)
	require.ErrorContains(t, err, "only NTFS volumes can be browsed")

	_, err = run(id+"/p2-C/missing", false)
	require.ErrorContains(t, err, "not found")
}

func TestImageRestoreFiles(t *testing.T) {
	ctx, env := repotesting.NewEnvironment(t, repotesting.FormatNotImportant)
	id := createNTFSTestImage(ctx, t, env)

	target := t.TempDir()

	restoreFiles := func(path string, overwrite bool) (string, error) {
		return captureStdout(t, func(out textOutput) error {
			return (&commandImageRestoreFiles{path: path, target: target, overwriteFiles: overwrite, parallel: 4, out: out}).run(ctx, env.Repository)
		})
	}

	// a file
	_, err := restoreFiles(id+"/p2-C/ones.bin", false)
	require.NoError(t, err)

	ones, err := os.ReadFile(filepath.Join(target, "ones.bin"))
	require.NoError(t, err)
	require.Len(t, ones, 2949120)

	// a folder, without its alternate data streams
	out, err := restoreFiles(id+"/p2-C/Folder A", false)
	require.NoError(t, err)
	t.Logf("%s", out)

	hello, err := os.ReadFile(filepath.Join(target, "Folder A", "Folder B", "Hello world text document.txt"))
	require.NoError(t, err)
	require.Equal(t, "Hello world!", string(hello))

	entries, err := os.ReadDir(filepath.Join(target, "Folder A", "Folder B"))
	require.NoError(t, err)
	require.Len(t, entries, 1)

	// existing files are refused unless --overwrite-files
	require.NoError(t, os.WriteFile(filepath.Join(target, "ones.bin"), []byte("changed"), 0o600))

	_, err = restoreFiles(id+"/p2-C/ones.bin", false)
	require.ErrorContains(t, err, "--overwrite-files")

	_, err = restoreFiles(id+"/p2-C/ones.bin", true)
	require.NoError(t, err)

	ones, err = os.ReadFile(filepath.Join(target, "ones.bin"))
	require.NoError(t, err)
	require.Len(t, ones, 2949120)

	_, err = restoreFiles(id, false)
	require.ErrorContains(t, err, "specify a volume")
}

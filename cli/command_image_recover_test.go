package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/internal/testlogging"
	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/snapshot"
	"github.com/kopia/kopia/snapshot/image"
	"github.com/kopia/kopia/snapshot/image/gpt"
	"github.com/kopia/kopia/snapshot/image/recoverymedia"
	"github.com/kopia/kopia/tests/testenv"
)

const recoverTestDiskGUID = "4c0a7f6e-2b1d-4e8a-9c3f-5d6e7f8a9b0c"

func recoverTestLayout() *image.Layout {
	return &image.Layout{
		DiskGUID:   recoverTestDiskGUID,
		SectorSize: 512,
		DiskSize:   48 << 20,
		Partitions: []image.Partition{
			{Number: 1, TypeGUID: image.PartitionTypeEFISystem, UniqueGUID: "4c0a7f6e-2b1d-4e8a-9c3f-000000000001", Offset: 1 << 20, Length: 8 << 20, Name: "EFI system partition"},
			{Number: 2, TypeGUID: image.PartitionTypeBasicData, UniqueGUID: "4c0a7f6e-2b1d-4e8a-9c3f-000000000002", Offset: 9 << 20, Length: 32 << 20, Name: "Basic data partition"},
		},
	}
}

func recoverTestBytes(seed uint64, n int) []byte {
	r := rand.New(rand.NewPCG(seed, seed)) //nolint:gosec
	b := make([]byte, n)

	for i := range b {
		b[i] = byte(r.Uint32())
	}

	return b
}

// createRecoverTestImage writes an image snapshot of two raw partitions
// into the test repository and returns their contents by partition number.
func createRecoverTestImage(t *testing.T, env *testenv.CLITest, host string) map[int][]byte {
	t.Helper()

	ctx := testlogging.Context(t)
	layout := recoverTestLayout()
	contents := map[int][]byte{1: recoverTestBytes(1, 8<<20), 2: recoverTestBytes(2, 32<<20)}

	clear(contents[2][8<<20 : 24<<20]) // free space

	path, err := image.DiskSourcePath(layout.DiskGUID)
	require.NoError(t, err)

	r, err := repo.Open(ctx, filepath.Join(env.ConfigDir, ".kopia.config"), testenv.TestRepoPassword, &repo.Options{})
	require.NoError(t, err)

	defer r.Close(ctx) //nolint:errcheck

	open := func(b []byte) func() (io.ReadCloser, error) {
		return func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(b)), nil }
	}

	require.NoError(t, repo.WriteSession(ctx, r, repo.WriteSessionOptions{Purpose: "test image"}, func(ctx context.Context, w repo.RepositoryWriter) error {
		_, _, err := image.Create(ctx, w, &image.CreateOptions{
			Source: snapshot.SourceInfo{Host: host, UserName: "tim", Path: path},
			Layout: layout,
			Volumes: []image.VolumeSource{
				{PartitionNumber: 1, Label: "esp", Kind: image.VolumeKindRaw, Size: int64(len(contents[1])), Open: open(contents[1])},
				{PartitionNumber: 2, Label: "data", Kind: image.VolumeKindRaw, Size: int64(len(contents[2])), Open: open(contents[2])},
			},
			Compressor: "zstd-fastest",
		})

		return err //nolint:wrapcheck
	}))

	return contents
}

func writeJSONFile(t *testing.T, v any) string {
	t.Helper()

	b, err := json.Marshal(v)
	require.NoError(t, err)

	p := filepath.Join(t.TempDir(), "file.json")
	require.NoError(t, os.WriteFile(p, b, 0o600))

	return p
}

func TestImageRecoverUnattendedToFile(t *testing.T) {
	env := testenv.NewCLITest(t, testenv.RepoFormatNotImportant, testenv.NewInProcRunner(t))
	env.RunAndExpectSuccess(t, "repo", "create", "filesystem", "--path", env.RepoDir)

	createRecoverTestImage(t, env, "other-pc")
	want := createRecoverTestImage(t, env, "desktop")

	info := filepath.Join(t.TempDir(), recoverymedia.InfoFileName)
	require.NoError(t, recoverymedia.WriteInfo(info, &recoverymedia.Info{
		Computer: "desktop",
		Sources:  []string{"tim@desktop:image:disk:" + recoverTestDiskGUID},
	}))

	target := filepath.Join(t.TempDir(), "restored.img")
	answers := writeJSONFile(t, map[string]any{"target": target, "confirm": true})

	out := env.RunAndExpectSuccess(t, "image", "recover", "--recovery-file", info, "--answers", answers, "--network-wait", "1s")
	joined := strings.Join(out, "\n")
	require.Contains(t, joined, "Kopia recovery media built on")
	require.Contains(t, joined, "of tim@desktop:image:disk:"+recoverTestDiskGUID, "the media's own computer is chosen by default")

	f, err := os.Open(target)
	require.NoError(t, err)

	defer f.Close()

	table, err := gpt.Read(f, 512)
	require.NoError(t, err)
	require.Len(t, table.Partitions, 2)

	for _, p := range recoverTestLayout().Partitions {
		got := make([]byte, p.Length)
		_, err := f.ReadAt(got, p.Offset)
		require.NoError(t, err)
		require.True(t, bytes.Equal(want[p.Number], got), "partition %v", p.Number)
	}
}

func TestImageRecoverRequiresConfirmation(t *testing.T) {
	env := testenv.NewCLITest(t, testenv.RepoFormatNotImportant, testenv.NewInProcRunner(t))
	env.RunAndExpectSuccess(t, "repo", "create", "filesystem", "--path", env.RepoDir)
	createRecoverTestImage(t, env, "desktop")

	target := filepath.Join(t.TempDir(), "restored.img")
	answers := writeJSONFile(t, map[string]any{"target": target})

	_, stderr := env.RunAndExpectFailure(t, "image", "recover", "--answers", answers)
	require.Contains(t, strings.Join(stderr, "\n"), "do not confirm")
	require.NoFileExists(t, target)
}

func TestImageRecoverNotConnected(t *testing.T) {
	env := testenv.NewCLITest(t, testenv.RepoFormatNotImportant, testenv.NewInProcRunner(t))

	_, stderr := env.RunAndExpectFailure(t, "image", "recover", "--target", filepath.Join(t.TempDir(), "x.img"))
	require.Contains(t, strings.Join(stderr, "\n"), "connect with 'kopia repository connect' first")
}

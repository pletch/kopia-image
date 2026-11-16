package server_test

import (
	"bytes"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/internal/repotesting"
	"github.com/kopia/kopia/internal/servertesting"
	"github.com/kopia/kopia/repo/object"
	"github.com/kopia/kopia/snapshot"
	"github.com/kopia/kopia/snapshot/image"
)

// mustCreateImageSnapshot stores go-ntfs's NTFS test image as partition 2 of
// an image snapshot and returns the snapshot root.
func mustCreateImageSnapshot(t *testing.T, env *repotesting.Environment) object.ID {
	t.Helper()

	out, err := exec.CommandContext(t.Context(), "go", "list", "-m", "-f", "{{.Dir}}", "www.velocidex.com/golang/go-ntfs").Output()
	if err != nil {
		t.Skipf("go-ntfs module not found: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(strings.TrimSpace(string(out)), "parser", "test_data", "test.ntfs.dd"))
	require.NoError(t, err)

	const diskGUID = "0f6ab3f2-1c3d-4e5f-8a9b-0c1d2e3f4a5b"

	path, err := image.DiskSourcePath(diskGUID)
	require.NoError(t, err)

	man, _, err := image.Create(t.Context(), env.RepositoryWriter, &image.CreateOptions{
		Source: snapshot.SourceInfo{Host: "h", UserName: "u", Path: path},
		Layout: &image.Layout{
			DiskGUID:   diskGUID,
			SectorSize: 512,
			DiskSize:   1 << 30,
			Partitions: []image.Partition{{
				Number: 2, TypeGUID: image.PartitionTypeBasicData, UniqueGUID: "11111111-2222-4333-8444-555555555552",
				Offset: 1 << 20, Length: 16 << 20, Name: "Basic data partition",
			}},
		},
		Volumes: []image.VolumeSource{{
			PartitionNumber: 2, Label: "C", Kind: image.VolumeKindNTFS, Size: int64(len(data)),
			Open: func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil },
		}},
	})
	require.NoError(t, err)

	return man.RootObjectID()
}

func TestWebDAVMounts_imageVolumes(t *testing.T) {
	ctx, env := repotesting.NewEnvironment(t, repotesting.FormatNotImportant)
	rootOID := mustCreateImageSnapshot(t, env)

	srvInfo := servertesting.StartServerContextWithOptions(ctx, t, env, servertesting.StartServerOptions{WebDAVMounts: true})
	share := srvInfo.BaseURL + "/webdav/" + rootOID.String() + "/"

	// the share root lists a folder for the NTFS volume next to its image
	status, body := uiWebDAVRequest(t, "PROPFIND", share, http.Header{"Depth": {"1"}})
	require.Equal(t, http.StatusMultiStatus, status)
	require.Contains(t, body, "/p2-C/")
	require.Contains(t, body, "p2-C.img")

	// files inside the volume download; repeated requests reuse the open volume
	file := share + "p2-C/" + url.PathEscape("Folder A") + "/" + url.PathEscape("Folder B") + "/" + url.PathEscape("Hello world text document.txt")

	for range 3 {
		status, body = uiWebDAVRequest(t, http.MethodGet, file, nil)
		require.Equal(t, http.StatusOK, status)
		require.Equal(t, "Hello world!", body)
	}
}

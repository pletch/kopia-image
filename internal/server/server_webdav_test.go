package server_test

import (
	"context"
	"io"
	"maps"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/internal/apiclient"
	"github.com/kopia/kopia/internal/mockfs"
	"github.com/kopia/kopia/internal/repotesting"
	"github.com/kopia/kopia/internal/serverapi"
	"github.com/kopia/kopia/internal/servertesting"
	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/object"
	"github.com/kopia/kopia/snapshot"
	"github.com/kopia/kopia/snapshot/upload"
)

func mustCreateWebDAVTestSnapshot(t *testing.T, ctx context.Context, env *repotesting.Environment) (snapshot.SourceInfo, object.ID) {
	t.Helper()

	si := env.LocalPathSourceInfo("/dummy/path")

	var rootOID object.ID

	require.NoError(t, repo.WriteSession(ctx, env.Repository, repo.WriteSessionOptions{Purpose: "Test"}, func(ctx context.Context, w repo.RepositoryWriter) error {
		dir := mockfs.NewDirectory()
		dir.AddFile("file1", []byte{1, 2, 3}, 0o644)
		dir.AddDir("dir1", 0o755).AddFile("file2", []byte{1, 2, 4}, 0o644)

		man, err := upload.NewUploader(w).Upload(ctx, dir, nil, si)
		require.NoError(t, err)

		_, err = snapshot.SaveSnapshot(ctx, w, man)
		require.NoError(t, err)

		rootOID = man.RootObjectID()

		return nil
	}))

	return si, rootOID
}

func webdavRequest(t *testing.T, method, url, username, password string, header http.Header) (int, string) {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), method, url, http.NoBody)
	require.NoError(t, err)

	maps.Copy(req.Header, header)

	if username != "" {
		req.SetBasicAuth(username, password)
	}

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)

	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	return resp.StatusCode, string(body)
}

func uiWebDAVRequest(t *testing.T, method, url string, header http.Header) (int, string) {
	t.Helper()

	return webdavRequest(t, method, url, servertesting.TestUIUsername, servertesting.TestUIPassword, header)
}

func TestWebDAVMounts(t *testing.T) {
	ctx, env := repotesting.NewEnvironment(t, repotesting.FormatNotImportant)
	si, rootOID := mustCreateWebDAVTestSnapshot(t, ctx, env)

	srvInfo := servertesting.StartServerContextWithOptions(ctx, t, env, servertesting.StartServerOptions{WebDAVMounts: true})

	cli, err := apiclient.NewKopiaAPIClient(apiclient.Options{
		BaseURL:  srvInfo.BaseURL,
		Username: servertesting.TestUIUsername,
		Password: servertesting.TestUIPassword,
	})
	require.NoError(t, err)
	require.NoError(t, cli.FetchCSRFTokenForTesting(ctx))

	depth1 := http.Header{"Depth": {"1"}}

	t.Run("AllSourcesListsSource", func(t *testing.T) {
		status, body := uiWebDAVRequest(t, "PROPFIND", srvInfo.BaseURL+"/webdav/all/", depth1)
		require.Equal(t, http.StatusMultiStatus, status)
		require.Contains(t, body, si.UserName+"@"+si.Host)
	})

	t.Run("RootOptionsAdvertisesDAV", func(t *testing.T) {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodOptions, srvInfo.BaseURL+"/", http.NoBody)
		require.NoError(t, err)

		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		resp.Body.Close()

		require.Equal(t, http.StatusOK, resp.StatusCode)
		require.Equal(t, "1, 2", resp.Header.Get("DAV"))
	})

	var mounted serverapi.MountedSnapshot

	require.NoError(t, cli.Post(ctx, "mounts", &serverapi.MountSnapshotRequest{Root: rootOID.String()}, &mounted))
	require.Equal(t, srvInfo.BaseURL+"/webdav/"+rootOID.String()+"/", mounted.Path)

	t.Run("SnapshotShareListsDirectory", func(t *testing.T) {
		status, body := uiWebDAVRequest(t, "PROPFIND", mounted.Path+"dir1/", depth1)
		require.Equal(t, http.StatusMultiStatus, status)
		require.Contains(t, body, "file2")
	})

	t.Run("SnapshotShareServesFile", func(t *testing.T) {
		status, body := uiWebDAVRequest(t, http.MethodGet, mounted.Path+"file1", nil)
		require.Equal(t, http.StatusOK, status)
		require.Equal(t, string([]byte{1, 2, 3}), body)
	})

	t.Run("SnapshotShareServesRange", func(t *testing.T) {
		status, body := uiWebDAVRequest(t, http.MethodGet, mounted.Path+"file1", http.Header{"Range": {"bytes=1-2"}})
		require.Equal(t, http.StatusPartialContent, status)
		require.Equal(t, string([]byte{2, 3}), body)
	})

	t.Run("UIGetsMountScript", func(t *testing.T) {
		status, body := uiWebDAVRequest(t, http.MethodGet, srvInfo.BaseURL+"/", nil)
		require.Equal(t, http.StatusOK, status)
		require.Contains(t, body, "data-kopia-webdav-open")
	})

	t.Run("UnmountForgetsShare", func(t *testing.T) {
		require.NoError(t, cli.Delete(ctx, "mounts/"+rootOID.String(), nil, nil, &serverapi.Empty{}))

		var mounts serverapi.MountedSnapshots

		require.NoError(t, cli.Get(ctx, "mounts", nil, &mounts))
		require.Empty(t, mounts.Items)
	})
}

// The server control user, which KopiaUI holds, unmounts by path, as
// Explorer's "Unmount" context menu entry does with a drive letter.
func TestMounts_ControlUnmountByPath(t *testing.T) {
	ctx, env := repotesting.NewEnvironment(t, repotesting.FormatNotImportant)
	_, rootOID := mustCreateWebDAVTestSnapshot(t, ctx, env)

	srvInfo := servertesting.StartServerContextWithOptions(ctx, t, env, servertesting.StartServerOptions{WebDAVMounts: true})

	ui, err := apiclient.NewKopiaAPIClient(apiclient.Options{
		BaseURL:  srvInfo.BaseURL,
		Username: servertesting.TestUIUsername,
		Password: servertesting.TestUIPassword,
	})
	require.NoError(t, err)
	require.NoError(t, ui.FetchCSRFTokenForTesting(ctx))

	control, err := apiclient.NewKopiaAPIClient(apiclient.Options{
		BaseURL:  srvInfo.BaseURL,
		Username: servertesting.TestServerControlUsername,
		Password: servertesting.TestServerControlPassword,
	})
	require.NoError(t, err)

	var mounted serverapi.MountedSnapshot

	require.NoError(t, ui.Post(ctx, "mounts", &serverapi.MountSnapshotRequest{Root: rootOID.String()}, &mounted))

	// another path is not unmounted
	err = control.Post(ctx, "control/unmount", &serverapi.UnmountPathRequest{Path: srvInfo.BaseURL + "/webdav/other/"}, &serverapi.MountedSnapshot{})
	require.ErrorContains(t, err, "nothing is mounted at")

	// the UI user cannot use the control API
	err = ui.Post(ctx, "control/unmount", &serverapi.UnmountPathRequest{Path: mounted.Path}, &serverapi.MountedSnapshot{})
	require.Error(t, err)

	// case and a trailing separator do not matter
	var unmounted serverapi.MountedSnapshot

	require.NoError(t, control.Post(ctx, "control/unmount", &serverapi.UnmountPathRequest{Path: strings.ToUpper(strings.TrimSuffix(mounted.Path, "/"))}, &unmounted))
	require.Equal(t, rootOID, unmounted.Root)

	var mounts serverapi.MountedSnapshots

	require.NoError(t, ui.Get(ctx, "mounts", nil, &mounts))
	require.Empty(t, mounts.Items)

	err = control.Post(ctx, "control/unmount", &serverapi.UnmountPathRequest{Path: mounted.Path}, &serverapi.MountedSnapshot{})
	require.ErrorContains(t, err, "nothing is mounted at")
}

func TestWebDAVMounts_Rejected(t *testing.T) {
	ctx, env := repotesting.NewEnvironment(t, repotesting.FormatNotImportant)
	_, rootOID := mustCreateWebDAVTestSnapshot(t, ctx, env)

	srvInfo := servertesting.StartServerContextWithOptions(ctx, t, env, servertesting.StartServerOptions{WebDAVMounts: true})
	shareURL := srvInfo.BaseURL + "/webdav/" + rootOID.String() + "/"

	t.Run("MissingCredentials", func(t *testing.T) {
		status, _ := webdavRequest(t, "PROPFIND", shareURL, "", "", nil)
		require.Equal(t, http.StatusUnauthorized, status)
	})

	t.Run("RepositoryUser", func(t *testing.T) {
		status, _ := webdavRequest(t, "PROPFIND", shareURL, servertesting.TestUsername+"@"+servertesting.TestHostname, servertesting.TestPassword, nil)
		require.Equal(t, http.StatusForbidden, status)
	})

	for _, method := range []string{http.MethodPut, http.MethodDelete, "MKCOL", "MOVE", "COPY", "PROPPATCH"} {
		t.Run("Method"+method, func(t *testing.T) {
			status, _ := uiWebDAVRequest(t, method, shareURL+"file1", nil)
			require.Equal(t, http.StatusMethodNotAllowed, status)
		})
	}

	t.Run("MissingShare", func(t *testing.T) {
		status, _ := uiWebDAVRequest(t, "PROPFIND", srvInfo.BaseURL+"/webdav/", nil)
		require.Equal(t, http.StatusNotFound, status)
	})

	t.Run("InvalidObjectID", func(t *testing.T) {
		status, _ := uiWebDAVRequest(t, "PROPFIND", srvInfo.BaseURL+"/webdav/not-an-oid/", nil)
		require.Equal(t, http.StatusNotFound, status)
	})
}

func TestWebDAVMounts_Disabled(t *testing.T) {
	ctx, env := repotesting.NewEnvironment(t, repotesting.FormatNotImportant)
	_, rootOID := mustCreateWebDAVTestSnapshot(t, ctx, env)

	srvInfo := servertesting.StartServerContextWithOptions(ctx, t, env, servertesting.StartServerOptions{})

	status, body := uiWebDAVRequest(t, "PROPFIND", srvInfo.BaseURL+"/webdav/"+rootOID.String()+"/", nil)
	require.NotEqual(t, http.StatusMultiStatus, status)
	require.NotContains(t, body, "file1", "snapshot contents must not be served")

	_, body = uiWebDAVRequest(t, http.MethodGet, srvInfo.BaseURL+"/", nil)
	require.NotContains(t, body, "data-kopia-webdav-open", "the UI must not get the WebDAV mount script")
}

func TestWebDAVMounts_BrowsePage(t *testing.T) {
	ctx, env := repotesting.NewEnvironment(t, repotesting.FormatNotImportant)
	_, rootOID := mustCreateWebDAVTestSnapshot(t, ctx, env)

	srvInfo := servertesting.StartServerContextWithOptions(ctx, t, env, servertesting.StartServerOptions{WebDAVMounts: true})
	shareURL := srvInfo.BaseURL + "/webdav/" + rootOID.String() + "/"
	acceptHTML := http.Header{"Accept": {"text/html,application/xhtml+xml"}}

	t.Run("FolderListing", func(t *testing.T) {
		status, body := uiWebDAVRequest(t, http.MethodGet, shareURL, acceptHTML)
		require.Equal(t, http.StatusOK, status)
		require.Contains(t, body, "Mount this folder")
		require.Contains(t, body, `href="dir1/"`)
		require.Contains(t, body, `href="file1"`)
		require.Contains(t, body, `net use * &#34;`+shareURL+`&#34; /user:`+servertesting.TestUIUsername)
	})

	t.Run("FolderWithoutSlashRedirects", func(t *testing.T) {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, strings.TrimSuffix(shareURL, "/"), http.NoBody)
		require.NoError(t, err)

		req.Header = acceptHTML
		req.SetBasicAuth(servertesting.TestUIUsername, servertesting.TestUIPassword)

		client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

		resp, err := client.Do(req)
		require.NoError(t, err)
		resp.Body.Close()

		require.Equal(t, http.StatusMovedPermanently, resp.StatusCode)
		require.Equal(t, "/webdav/"+rootOID.String()+"/", resp.Header.Get("Location"))
	})

	t.Run("FileIsServedNotListed", func(t *testing.T) {
		status, body := uiWebDAVRequest(t, http.MethodGet, shareURL+"file1", acceptHTML)
		require.Equal(t, http.StatusOK, status)
		require.Equal(t, string([]byte{1, 2, 3}), body)
	})

	t.Run("SharesPage", func(t *testing.T) {
		status, body := uiWebDAVRequest(t, http.MethodGet, srvInfo.BaseURL+"/webdav/", acceptHTML)
		require.Equal(t, http.StatusOK, status)
		require.Contains(t, body, `href="all/"`)
	})

	t.Run("WindowsScript", func(t *testing.T) {
		status, body := uiWebDAVRequest(t, http.MethodGet, shareURL+"dir1/?mount=windows", nil)
		require.Equal(t, http.StatusOK, status)
		require.Contains(t, body, `net use %DRIVE% "`+shareURL+`dir1/" /user:`+servertesting.TestUIUsername+` /persistent:no`)
	})

	t.Run("LinuxScript", func(t *testing.T) {
		status, body := uiWebDAVRequest(t, http.MethodGet, shareURL+"dir1/?mount=linux", nil)
		require.Equal(t, http.StatusOK, status)
		require.Contains(t, body, "url='dav"+strings.TrimPrefix(shareURL, "http")+"dir1/'")
	})

	t.Run("UnknownScript", func(t *testing.T) {
		status, _ := uiWebDAVRequest(t, http.MethodGet, shareURL+"?mount=mac", nil)
		require.Equal(t, http.StatusBadRequest, status)
	})
}

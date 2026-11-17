package webdavmount_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/net/webdav"

	"github.com/kopia/kopia/internal/mockfs"
	"github.com/kopia/kopia/internal/webdavmount"
)

func propfind(t *testing.T, h http.Handler, path string) string {
	t.Helper()

	r := httptest.NewRequestWithContext(t.Context(), "PROPFIND", path, strings.NewReader(""))
	r.Header.Set("Depth", "1")

	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	require.Equal(t, http.StatusMultiStatus, w.Code, w.Body.String())

	return w.Body.String()
}

// Listing a folder must not list each of its subfolders: an image volume
// folder such as C:\Windows has subfolders with tens of thousands of entries.
func TestPropfindListsOnlyTheRequestedFolder(t *testing.T) {
	root := mockfs.NewDirectory()
	parent := root.AddDir("parent", 0o755)

	var parentReads, subdirReads atomic.Int32

	parent.OnReaddir(func() { parentReads.Add(1) })

	for i := range 20 {
		sub := parent.AddDir(fmt.Sprintf("sub%v", i), 0o755)
		sub.AddFile("file.txt", []byte("x"), 0o644)
		sub.OnReaddir(func() { subdirReads.Add(1) })
	}

	parent.AddFile("file.txt", []byte("hello"), 0o644)

	h := &webdav.Handler{
		FileSystem: webdavmount.WebDAVFS(root),
		LockSystem: webdav.NewMemLS(),
	}

	body := propfind(t, h, "/parent/")

	require.Contains(t, body, "/parent/sub19/")
	require.Contains(t, body, "/parent/file.txt")
	require.EqualValues(t, 1, parentReads.Load())
	require.EqualValues(t, 0, subdirReads.Load())

	// a subfolder is listed when it is itself requested
	require.Contains(t, propfind(t, h, "/parent/sub3/"), "/parent/sub3/file.txt")
	require.EqualValues(t, 1, subdirReads.Load())
}

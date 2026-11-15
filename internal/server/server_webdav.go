package server

import (
	"context"
	"net/http"
	"strings"

	"github.com/gorilla/mux"
	"golang.org/x/net/webdav"

	"github.com/kopia/kopia/fs"
	"github.com/kopia/kopia/internal/webdavmount"
	"github.com/kopia/kopia/repo/object"
	"github.com/kopia/kopia/snapshot/snapshotfs"
)

const (
	webdavPathPrefix = "/webdav/"

	// webdavAllSourcesShare is the share that exposes all snapshots of all sources.
	webdavAllSourcesShare = "all"
)

// webdavReadOnlyMethods lists the methods a read-only WebDAV client needs.
// LOCK and UNLOCK are included because Windows Explorer locks files before opening them.
//
//nolint:gochecknoglobals
var webdavReadOnlyMethods = map[string]bool{
	http.MethodGet:     true,
	http.MethodHead:    true,
	http.MethodOptions: true,
	"PROPFIND":         true,
	"LOCK":             true,
	"UNLOCK":           true,
}

// setupWebDAVHandlers exposes snapshots as a read-only WebDAV tree under /webdav/:
//
//	/webdav/all/<user@host>/<path>/<snapshot time>/...  - all snapshots of all sources
//	/webdav/<root object ID>/...                         - a single snapshot directory
func (s *Server) setupWebDAVHandlers(m *mux.Router) {
	// The Windows WebDAV client probes the server root with OPTIONS before mapping a drive.
	m.HandleFunc("/", handleWebDAVRootOptions).Methods(http.MethodOptions)
	m.PathPrefix(webdavPathPrefix).HandlerFunc(s.requireAuth(csrfTokenNotRequired, s.handleWebDAV))
}

func handleWebDAVRootOptions(w http.ResponseWriter, _ *http.Request) {
	setWebDAVOptionsHeaders(w)
	w.WriteHeader(http.StatusOK)
}

func setWebDAVOptionsHeaders(w http.ResponseWriter) {
	w.Header().Set("DAV", "1, 2")
	w.Header().Set("Ms-Author-Via", "DAV")
	w.Header().Set("Allow", "OPTIONS, GET, HEAD, PROPFIND, LOCK, UNLOCK")
}

func (s *Server) handleWebDAV(ctx context.Context, rc requestContext) {
	if !requireUIUser(ctx, rc) {
		http.Error(rc.w, "WebDAV access denied.\n", http.StatusForbidden)
		return
	}

	if !webdavReadOnlyMethods[rc.req.Method] {
		setWebDAVOptionsHeaders(rc.w)
		http.Error(rc.w, "Snapshots are read-only.\n", http.StatusMethodNotAllowed)

		return
	}

	if rc.rep == nil {
		http.Error(rc.w, "Repository not connected.\n", http.StatusServiceUnavailable)
		return
	}

	share, _, _ := strings.Cut(strings.TrimPrefix(rc.req.URL.Path, webdavPathPrefix), "/")

	var entry fs.Directory

	switch share {
	case "":
		if wantsWebDAVBrowsePage(rc.req) {
			serveWebDAVSharesPage(rc.w, rc.req)
			return
		}

		http.Error(rc.w, "Specify a share, such as "+webdavPathPrefix+webdavAllSourcesShare+"/\n", http.StatusNotFound)

		return

	case webdavAllSourcesShare:
		entry = snapshotfs.AllSourcesEntry(rc.rep)

	default:
		oid, err := object.ParseID(share)
		if err != nil {
			http.Error(rc.w, "Invalid snapshot root object ID.\n", http.StatusNotFound)
			return
		}

		entry = snapshotfs.DirectoryEntry(rc.rep, oid, nil)
	}

	prefix := webdavPathPrefix + share
	fsys := webdavmount.WebDAVFS(entry)

	if wantsWebDAVBrowsePage(rc.req) && serveWebDAVBrowsePage(ctx, rc.w, rc.req, prefix, fsys) {
		return
	}

	h := &webdav.Handler{
		Prefix:     prefix,
		FileSystem: fsys,
		LockSystem: s.webdavLocks,
	}

	h.ServeHTTP(rc.w, rc.req)
}

// webdavShareURL returns the URL under which the server exposes the given snapshot directory,
// using the scheme and host the client used to reach the server.
func webdavShareURL(r *http.Request, oid object.ID) string {
	return requestScheme(r) + "://" + r.Host + webdavPathPrefix + oid.String() + "/"
}

// webdavShare is the mount controller of a snapshot that the server exposes over WebDAV.
// Nothing is mounted on the server, so unmounting only forgets the share.
type webdavShare struct {
	url  string
	done chan struct{}
}

func newWebDAVShare(url string) webdavShare {
	return webdavShare{url: url, done: make(chan struct{})}
}

func (c webdavShare) Unmount(_ context.Context) error {
	return nil
}

func (c webdavShare) MountPath() string {
	return c.url
}

func (c webdavShare) Done() <-chan struct{} {
	return c.done
}

package server

import (
	"context"
	_ "embed"
	"fmt"
	"html/template"
	"net/http"
	"os"
	"path"
	"slices"
	"strings"
	"time"

	"golang.org/x/net/webdav"
)

// The browse page is what a web browser gets for a WebDAV folder: a listing of the
// folder plus the commands and scripts that mount it as a drive on Windows or Linux.

//go:embed webdav_browse.html
var webdavBrowseHTML string

//nolint:gochecknoglobals
var webdavBrowseTemplate = template.Must(template.New("browse").Parse(webdavBrowseHTML))

const (
	mountScriptQueryParam = "mount"
	mountScriptWindows    = "windows"
	mountScriptLinux      = "linux"

	cmdEmptyLine = "echo."
)

type webdavBrowseEntry struct {
	Name    string
	Href    string
	IsDir   bool
	Size    int64
	ModTime time.Time
}

type webdavBrowseCrumb struct {
	Name string
	Href string
}

type webdavBrowsePage struct {
	Path            string
	Crumbs          []webdavBrowseCrumb
	Entries         []webdavBrowseEntry
	MountURL        string
	DavURL          string
	NetUseCommand   string
	GioCommand      string
	FstabLine       string
	WindowsDownload string
	LinuxDownload   string
}

// wantsWebDAVBrowsePage reports whether the request comes from a web browser rather than a WebDAV client.
func wantsWebDAVBrowsePage(r *http.Request) bool {
	if r.Method != http.MethodGet {
		return false
	}

	return r.URL.Query().Has(mountScriptQueryParam) || strings.Contains(r.Header.Get("Accept"), "text/html")
}

// serveWebDAVBrowsePage serves the browse page or a mount script for the folder at the request path.
// It returns false without writing a response when the path is not a folder, so the WebDAV handler
// can serve the file.
func serveWebDAVBrowsePage(ctx context.Context, w http.ResponseWriter, r *http.Request, prefix string, fsys webdav.FileSystem) bool {
	dirPath := strings.TrimPrefix(r.URL.Path, prefix)

	f, err := fsys.OpenFile(ctx, dirPath, os.O_RDONLY, 0)
	if err != nil {
		return false
	}

	defer f.Close() //nolint:errcheck

	st, err := f.Stat()
	if err != nil || !st.IsDir() {
		return false
	}

	if !strings.HasSuffix(r.URL.Path, "/") {
		http.Redirect(w, r, strictPathEscape(r.URL.Path+"/"), http.StatusMovedPermanently)
		return true
	}

	infos, err := f.Readdir(-1)
	if err != nil {
		http.Error(w, "Unable to list folder.\n", http.StatusInternalServerError)
		return true
	}

	serveWebDAVBrowseResponse(w, r, infos)

	return true
}

// serveWebDAVSharesPage serves the browse page of /webdav/, which lists the available shares.
func serveWebDAVSharesPage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != webdavPathPrefix {
		http.Redirect(w, r, webdavPathPrefix, http.StatusMovedPermanently)
		return
	}

	serveWebDAVBrowseResponse(w, r, []os.FileInfo{shareInfo(webdavAllSourcesShare)})
}

func serveWebDAVBrowseResponse(w http.ResponseWriter, r *http.Request, infos []os.FileInfo) {
	page := newWebDAVBrowsePage(r, infos)

	user, _, _ := r.BasicAuth()

	switch r.URL.Query().Get(mountScriptQueryParam) {
	case "":
		w.Header().Set("Content-Type", "text/html; charset=utf-8")

		if err := webdavBrowseTemplate.Execute(w, page); err != nil {
			userLog(r.Context()).Debugf("unable to render browse page: %v", err)
		}

	case mountScriptWindows:
		serveMountScript(w, "mount-kopia-snapshot.cmd", windowsMountScript(page.MountURL, user))

	case mountScriptLinux:
		serveMountScript(w, "mount-kopia-snapshot.sh", linuxMountScript(page.DavURL))

	default:
		http.Error(w, "Unknown mount script.\n", http.StatusBadRequest)
	}
}

func newWebDAVBrowsePage(r *http.Request, infos []os.FileInfo) webdavBrowsePage {
	user, _, _ := r.BasicAuth()

	escapedPath := strictPathEscape(r.URL.Path)
	mountURL := requestScheme(r) + "://" + r.Host + escapedPath
	davURL := "dav" + strings.TrimPrefix(mountURL, "http")

	page := webdavBrowsePage{
		Path:            r.URL.Path,
		Crumbs:          webdavBrowseCrumbs(r.URL.Path),
		MountURL:        mountURL,
		DavURL:          davURL,
		NetUseCommand:   fmt.Sprintf(`net use * "%v"%v /persistent:no`, mountURL, netUseUserArg(user)),
		GioCommand:      fmt.Sprintf(`gio mount '%v'`, davURL),
		FstabLine:       mountURL + " /mnt/kopia davfs ro,user,noauto,_netdev 0 0",
		WindowsDownload: escapedPath + "?" + mountScriptQueryParam + "=" + mountScriptWindows,
		LinuxDownload:   escapedPath + "?" + mountScriptQueryParam + "=" + mountScriptLinux,
	}

	for _, fi := range infos {
		href := strictPathEscape(fi.Name())
		if fi.IsDir() {
			href += "/"
		}

		page.Entries = append(page.Entries, webdavBrowseEntry{
			Name:    fi.Name(),
			Href:    href,
			IsDir:   fi.IsDir(),
			Size:    fi.Size(),
			ModTime: fi.ModTime(),
		})
	}

	slices.SortFunc(page.Entries, func(a, b webdavBrowseEntry) int {
		if a.IsDir != b.IsDir {
			if a.IsDir {
				return -1
			}

			return 1
		}

		return strings.Compare(a.Name, b.Name)
	})

	return page
}

// webdavBrowseCrumbs returns the links to the folders above dirPath, starting at /webdav/.
func webdavBrowseCrumbs(dirPath string) []webdavBrowseCrumb {
	var crumbs []webdavBrowseCrumb

	href := "/"

	for name := range strings.SplitSeq(strings.Trim(dirPath, "/"), "/") {
		if name == "" {
			continue
		}

		href = path.Join(href, name) + "/"
		crumbs = append(crumbs, webdavBrowseCrumb{Name: name, Href: strictPathEscape(href)})
	}

	return crumbs
}

func requestScheme(r *http.Request) string {
	if r.TLS != nil {
		return "https"
	}

	return "http"
}

// strictPathEscape percent-encodes every byte of p except unreserved characters and slashes.
// The result is safe to embed in URLs, HTML, and in quoted shell and cmd.exe strings
// (where only '%' still needs escaping).
func strictPathEscape(p string) string {
	var sb strings.Builder

	for i := range len(p) {
		c := p[i]

		if isUnreservedPathByte(c) {
			sb.WriteByte(c)
			continue
		}

		fmt.Fprintf(&sb, "%%%02X", c)
	}

	return sb.String()
}

func isUnreservedPathByte(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	default:
		return strings.IndexByte("-._~/", c) >= 0
	}
}

// netUseUserArg returns the /user argument of 'net use', or nothing when the user name
// contains characters that would need quoting in cmd.exe.
func netUseUserArg(user string) string {
	isSafe := func(c rune) bool {
		return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("-._@", c)
	}

	if user == "" || strings.ContainsFunc(user, func(c rune) bool { return !isSafe(c) }) {
		return ""
	}

	return " /user:" + user
}

// windowsMountScript maps mountURL to the last free drive letter, opens it in Explorer and
// shows how to disconnect it. 'net use' prompts for the password.
func windowsMountScript(mountURL, user string) string {
	lines := []string{
		"@echo off",
		"rem Maps a Kopia snapshot folder served over WebDAV to a drive letter.",
		"setlocal",
		"set DRIVE=",
		"for %%L in (Z Y X W V U T S R Q P O N M L K J I H G F E D) do if not defined DRIVE if not exist %%L:\\ set DRIVE=%%L:",
		"if not defined DRIVE (echo No free drive letter. & pause & exit /b 1)",
		// '%' is the only character left in a strictly escaped URL that cmd.exe interprets.
		fmt.Sprintf(`net use %%DRIVE%% "%v"%v /persistent:no`, strings.ReplaceAll(mountURL, "%", "%%"), netUseUserArg(user)),
		"if errorlevel 1 goto failed",
		"start \"\" %DRIVE%\\",
		cmdEmptyLine,
		"echo The snapshot folder is mapped to %DRIVE% until you sign out.",
		"echo To disconnect it, right-click the drive in Explorer and choose Disconnect, or run:",
		"echo     net use %DRIVE% /delete",
		cmdEmptyLine,
		"pause",
		"exit /b 0",
		"",
		":failed",
		cmdEmptyLine,
		"echo Mapping the drive failed. Check that:",
		"echo  - the WebClient service is running, or start it as administrator: net start WebClient",
		"echo  - this PC trusts the server's TLS certificate, and the URL uses the host name in it",
		"echo  - the user name and password are your Kopia server login",
		"echo Windows also refuses files over 50 MB unless the WebClient FileSizeLimitInBytes registry value is raised.",
		"pause",
		"exit /b 1",
		"",
	}

	return strings.Join(lines, "\r\n")
}

// linuxMountScript mounts davURL with GVfs and opens it in the file manager.
func linuxMountScript(davURL string) string {
	return fmt.Sprintf(`#!/bin/sh
# Mounts a Kopia snapshot folder served over WebDAV with GVfs (GNOME Files, Nemo, Caja, Thunar).
set -e
url='%v'
gio mount "$url" || true
xdg-open "$url"
echo "To unmount: gio mount -u '$url'"
`, davURL)
}

func serveMountScript(w http.ResponseWriter, fileName, body string) {
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="`+fileName+`"`)
	_, _ = w.Write([]byte(body))
}

// shareInfo describes a share as a folder in the /webdav/ listing.
type shareInfo string

func (s shareInfo) Name() string       { return string(s) }
func (s shareInfo) Size() int64        { return 0 }
func (s shareInfo) Mode() os.FileMode  { return os.ModeDir | 0o555 } //nolint:mnd
func (s shareInfo) ModTime() time.Time { return time.Time{} }
func (s shareInfo) IsDir() bool        { return true }
func (s shareInfo) Sys() any           { return nil }

package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"html/template"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	windowNameHeader = "X-Wails-Window-Name"
	windowNamePrefix = "repo:"
	shimPrefix       = "/__kopiaui/"
)

//go:embed shim.js
var shimJS []byte

// startupPage is shown instead of the web UI while the repository server is
// starting (reloading every second) or after it failed to start or exited.
var startupPage = template.Must(template.New("startup").Parse(`<!doctype html>
<html><head><meta charset="utf-8">
{{if not .Failed}}<meta http-equiv="refresh" content="1">{{end}}
<title>{{if .Failed}}Kopia server problem{{else}}KopiaUI is Loading...{{end}}</title>
<style>
:root{--bg:#fff;--fg:#333;--muted:#666;--border:#ddd;--code:#f5f5f5;--accent:#0d6efd;--danger:#b02a37}
@media (prefers-color-scheme:dark){:root{--bg:#1e1e1e;--fg:#ddd;--muted:#aaa;--border:#444;--code:#2a2a2a;--accent:#6ea8fe;--danger:#ea868f}}
body{margin:0;background:var(--bg);color:var(--fg);font:15px/1.5 system-ui,sans-serif}
.wrap{max-width:860px;margin:0 auto;padding:32px 24px}
.center{display:flex;align-items:center;justify-content:center;height:100vh;color:var(--muted)}
h1{font-size:20px;margin:0 0 8px;color:var(--danger)}
p{margin:0 0 16px}
.actions{display:flex;gap:8px;margin:0 0 20px}
button{font:inherit;padding:6px 14px;border-radius:6px;border:1px solid var(--border);background:var(--bg);color:var(--fg);cursor:pointer}
button.primary{background:var(--accent);border-color:var(--accent);color:#fff}
pre{background:var(--code);border:1px solid var(--border);border-radius:6px;padding:12px;font-size:12px;line-height:1.4;max-height:55vh;overflow:auto;white-space:pre-wrap;word-break:break-all}
</style></head>
<body>
{{if .Failed}}<div class="wrap">
<h1>The Kopia server for this repository is not available</h1>
<p>{{.Reason}}</p>
<div class="actions">
<form method="post" action="/__kopiaui/restart"><button class="primary" type="submit">Restart Server</button></form>
<form method="post" action="/__kopiaui/open-log"><button type="submit">Open Log File</button></form>
</div>
{{if .Logs}}<p>Last lines from the server:</p><pre>{{range .Logs}}{{.}}
{{end}}</pre>{{end}}
</div>
{{else}}<div class="center">Starting Kopia server&hellip;{{if .Elapsed}} ({{.Elapsed}}){{end}}</div>{{end}}
</body></html>`))

type startupPageData struct {
	Failed  bool
	Reason  string
	Elapsed string
	Logs    []string
}

func writeStartupPage(w http.ResponseWriter, p startupProblem) {
	data := startupPageData{Failed: p.failed, Reason: p.reason, Logs: p.logs}

	if !p.failed && p.elapsed >= 3*time.Second {
		data.Elapsed = p.elapsed.Truncate(time.Second).String()
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")

	if err := startupPage.Execute(w, data); err != nil {
		slog.Warn("unable to render startup page", "err", err)
	}
}

// proxyHandler is the Wails asset handler. Every request from a repository
// window carries that window's name, which identifies the repository server
// to forward to.
//
// Cookies are kept by the proxy, per repository server, rather than by the
// webview: WebView2 does not store cookies set by responses that Wails serves
// through request interception, and all windows share one origin anyway.
// There is one window per repository, so a per-server jar is equivalent to
// the browser's cookie store in Electron KopiaUI.
type proxyHandler struct {
	host *shell
}

// repoIDFromRequest returns the repository a request is for: the one the
// window shows, or for a window named after a repository, that one.
func (h *proxyHandler) repoIDFromRequest(r *http.Request) string {
	name := r.Header.Get(windowNameHeader)

	if name == mainWindowName {
		return h.host.configs.activeRepository()
	}

	return strings.TrimPrefix(name, windowNamePrefix)
}

func (h *proxyHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	repoID := h.repoIDFromRequest(r)

	if strings.HasPrefix(r.URL.Path, shimPrefix) {
		h.serveShim(w, r, repoID)
		return
	}

	srv := h.host.server(repoID)
	if srv == nil {
		http.Error(w, "unknown repository window", http.StatusNotFound)
		return
	}

	address, password, transport, ok := srv.connection()
	if !ok {
		if r.Method == http.MethodGet && !strings.HasPrefix(r.URL.Path, "/api/") {
			writeStartupPage(w, srv.startupProblem())

			return
		}

		http.Error(w, "kopia server is not available", http.StatusServiceUnavailable)

		return
	}

	target, err := url.Parse(address)
	if err != nil {
		http.Error(w, "invalid server address", http.StatusBadGateway)
		return
	}

	rp := &httputil.ReverseProxy{
		Transport: transport,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.Host = target.Host

			pr.Out.SetBasicAuth(uiUser, password)

			// Let the transport negotiate compression so it transparently
			// decompresses HTML that needs the shim injected.
			pr.Out.Header.Del("Accept-Encoding")
			pr.Out.Header.Del(windowNameHeader)
			pr.Out.Header.Del("x-wails-window-id")
			pr.Out.Header.Del("Origin")
			pr.Out.Header.Del("Referer")

			pr.Out.Header.Del("Cookie")
			for _, c := range srv.cookies() {
				pr.Out.AddCookie(c)
			}
		},
		ModifyResponse: func(resp *http.Response) error {
			srv.storeCookies(resp.Cookies())
			resp.Header.Del("Set-Cookie")

			return injectShim(resp)
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			slog.Warn("proxy error", "repo", repoID, "path", r.URL.Path, "err", err)
			http.Error(w, "unable to reach kopia server: "+err.Error(), http.StatusBadGateway)
		},
	}

	rp.ServeHTTP(w, r)
}

// injectShim adds the window.kopiaUI shim to HTML pages so the web UI shows the
// directory picker and "Browse" buttons it normally only shows in KopiaUI.
func injectShim(resp *http.Response) error {
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") {
		return nil
	}

	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if err != nil {
		return err
	}

	tag := []byte(`<script src="` + shimPrefix + `shim.js"></script></head>`)
	body = bytes.Replace(body, []byte("</head>"), tag, 1)

	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	resp.Header.Set("Content-Length", strconv.Itoa(len(body)))
	resp.Header.Del("Content-Encoding")

	return nil
}

// localDirectory returns the cleaned path of an existing folder on a local
// drive. It refuses relative paths, URLs, and network (UNC) and device paths,
// which would make the system contact another computer, and anything that is
// not a folder, which the system would run or open with another program.
func localDirectory(path string) (string, error) {
	if path == "" || strings.Contains(path, "://") {
		return "", errNotLocalDirectory
	}

	if runtime.GOOS == "windows" {
		// "C:\..." only: UNC and device paths start with two separators
		path = filepath.FromSlash(path)

		// a mounted snapshot is reported as a bare drive such as "Z:": show
		// its root, not the drive's current folder
		if len(path) == 2 && path[1] == ':' {
			path += `\`
		}

		if vol := filepath.VolumeName(path); len(vol) != 2 || vol[1] != ':' || !strings.HasPrefix(path[2:], `\`) {
			return "", errNotLocalDirectory
		}
	} else if !filepath.IsAbs(path) {
		return "", errNotLocalDirectory
	}

	path = filepath.Clean(path)

	if st, err := os.Stat(path); err != nil || !st.IsDir() {
		return "", errNotLocalDirectory
	}

	return path, nil
}

var (
	errNotLocalDirectory = errors.New("not an existing local folder")
	errNotWebURL         = errors.New("not an http or https URL")
)

// externalURL returns rawURL if it is an absolute http or https URL.
func externalURL(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", errNotWebURL
	}

	return u.String(), nil
}

// allowShimAction reports whether a request may trigger an action of this
// program, such as opening a folder. Actions need a POST that the page itself
// sent: browsers label requests from other sites with Sec-Fetch-Site, and
// such a request must not be able to act here even if it reaches the proxy.
func allowShimAction(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return false
	}

	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" {
		http.Error(w, "cross-site request", http.StatusForbidden)
		return false
	}

	return true
}

// decodeJSONRequest decodes a small JSON request body. It insists on the JSON
// content type, which a page of another site cannot send without permission.
func decodeJSONRequest(w http.ResponseWriter, r *http.Request, v any) bool {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		http.Error(w, "content type must be application/json", http.StatusUnsupportedMediaType)
		return false
	}

	if err := json.NewDecoder(io.LimitReader(r.Body, maxShimRequestSize)).Decode(v); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return false
	}

	return true
}

const maxShimRequestSize = 64 * 1024

// serveElevated runs an action that asks for administrator rights, after
// decoding the request body into req. The JSON content type that decoding
// insists on keeps other sites out, so actions without settings take an
// empty object. It answers {"status": done}, {"status":"canceled"} when the
// person declines the prompt, and an error otherwise.
func serveElevated[T any](w http.ResponseWriter, r *http.Request, done string, act func(req *T) error) {
	var req T

	if !decodeJSONRequest(w, r, &req) {
		return
	}

	status := done

	err := act(&req)

	switch {
	case errors.Is(err, errElevationCanceled):
		status = "canceled"
	case errors.Is(err, errElevationBusy):
		http.Error(w, err.Error(), http.StatusConflict)
		return
	case errors.Is(err, errInvalidSchedule):
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	case err != nil:
		slog.Warn("action as administrator failed", "path", r.URL.Path, "err", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)

		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": status})
}

func (h *proxyHandler) serveShim(w http.ResponseWriter, r *http.Request, repoID string) {
	action := strings.TrimPrefix(r.URL.Path, shimPrefix)

	if action != "shim.js" && !allowShimAction(w, r) {
		return
	}

	switch action {
	case "shim.js":
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		_, _ = w.Write(shimJS)

	case "repositories":
		var req struct{}

		if !decodeJSONRequest(w, r, &req) {
			return
		}

		w.Header().Set("Content-Type", "application/json")

		if err := json.NewEncoder(w).Encode(h.host.repositories(repoID)); err != nil {
			slog.Warn("unable to list repositories", "err", err)
		}

	case "switch-repository":
		var req struct {
			ID string `json:"id"`
		}

		if !decodeJSONRequest(w, r, &req) {
			return
		}

		if !h.host.configs.has(req.ID) {
			http.Error(w, "unknown repository", http.StatusNotFound)
			return
		}

		// the page reloads itself once this answers
		h.host.switchRepository(req.ID)
		go h.host.updateWindowTitle()

		w.WriteHeader(http.StatusNoContent)

	case "window-colors":
		var req titleBarColors

		if !decodeJSONRequest(w, r, &req) {
			return
		}

		if err := h.host.setTitleBarColors(req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		w.WriteHeader(http.StatusNoContent)

	case "add-repository":
		var req struct{}

		if !decodeJSONRequest(w, r, &req) {
			return
		}

		id := h.host.configs.addNew()

		h.host.startServer(id)
		h.host.switchRepository(id)

		go h.host.updateWindowTitle()

		w.WriteHeader(http.StatusNoContent)

	case "select-dir":
		path, err := h.host.selectDirectory()
		if err != nil {
			slog.Warn("select directory failed", "err", err)
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"path": path})

	case "restart":
		if h.host.server(repoID) == nil {
			http.Error(w, "unknown repository window", http.StatusNotFound)
			return
		}

		slog.Info("restarting server from error page", "repo", repoID)
		h.host.startServer(repoID)
		http.Redirect(w, r, "/", http.StatusSeeOther)

	case "open-log":
		h.host.openPath(h.host.logPath)
		http.Redirect(w, r, "/", http.StatusSeeOther)

	case "browse-dir":
		var req struct {
			Path string `json:"path"`
		}

		if !decodeJSONRequest(w, r, &req) {
			return
		}

		if err := h.host.browseDirectory(req.Path); err != nil {
			slog.Warn("not opening path requested by the web UI", "path", req.Path, "err", err)
			http.Error(w, err.Error(), http.StatusBadRequest)

			return
		}

		w.WriteHeader(http.StatusNoContent)

	case "open-url":
		var req struct {
			URL string `json:"url"`
		}

		if !decodeJSONRequest(w, r, &req) {
			return
		}

		if err := h.host.openExternalURL(req.URL); err != nil {
			slog.Warn("not opening URL requested by the web UI", "url", req.URL, "err", err)
			http.Error(w, err.Error(), http.StatusBadRequest)

			return
		}

		w.WriteHeader(http.StatusNoContent)

	case "run-image-task":
		serveElevated(w, r, "started", func(*struct{}) error { return h.host.runImageTaskElevated() })

	case "install-image-schedule":
		serveElevated(w, r, "done", func(s *imageScheduleSettings) error { return h.host.installImageSchedule(repoID, s) })

	case "remove-image-schedule":
		serveElevated(w, r, "done", func(*struct{}) error { return h.host.removeImageSchedule() })

	case "update-image-schedule":
		serveElevated(w, r, "done", func(*struct{}) error { return h.host.updateImageSchedule() })

	case "pause-image-schedule":
		serveElevated(w, r, "done", func(*struct{}) error { return h.host.pauseImageSchedule(false) })

	case "resume-image-schedule":
		serveElevated(w, r, "done", func(*struct{}) error { return h.host.pauseImageSchedule(true) })

	case "create-image":
		serveElevated(w, r, "started", func(req *struct {
			Volumes []string `json:"volumes"`
		},
		) error {
			return h.host.startImage(repoID, req.Volumes)
		})

	case "stop-image":
		serveElevated(w, r, "done", func(*struct{}) error { return h.host.stopImage() })

	case "create-recovery-media":
		serveElevated(w, r, "started", func(req *recoveryMediaRequest) error {
			return h.host.startRecoveryMedia(repoID, req)
		})

	default:
		http.NotFound(w, r)
	}
}

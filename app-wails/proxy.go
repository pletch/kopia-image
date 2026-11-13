package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	windowNameHeader = "x-wails-window-name"
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

func repoIDFromRequest(r *http.Request) string {
	return strings.TrimPrefix(r.Header.Get(windowNameHeader), windowNamePrefix)
}

func (h *proxyHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	repoID := repoIDFromRequest(r)

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

func (h *proxyHandler) serveShim(w http.ResponseWriter, r *http.Request, repoID string) {
	switch strings.TrimPrefix(r.URL.Path, shimPrefix) {
	case "shim.js":
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		_, _ = w.Write(shimJS)

	case "select-dir":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		path, err := h.host.selectDirectory(repoID)
		if err != nil {
			slog.Warn("select directory failed", "err", err)
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"path": path})

	case "restart":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		if h.host.server(repoID) == nil {
			http.Error(w, "unknown repository window", http.StatusNotFound)
			return
		}

		slog.Info("restarting server from error page", "repo", repoID)
		h.host.startServer(repoID)
		http.Redirect(w, r, "/", http.StatusSeeOther)

	case "open-log":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		h.host.browseDirectory(h.host.logPath)
		http.Redirect(w, r, "/", http.StatusSeeOther)

	case "browse-dir":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req struct {
			Path string `json:"path"`
		}

		if err := json.NewDecoder(io.LimitReader(r.Body, 64*1024)).Decode(&req); err != nil || req.Path == "" {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}

		h.host.browseDirectory(req.Path)
		w.WriteHeader(http.StatusNoContent)

	default:
		http.NotFound(w, r)
	}
}
